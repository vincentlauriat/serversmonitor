// Package hub wires store, ingest, alerts and server together and runs the periodic jobs.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vincentlauriat/serversmonitor/deploy"
	"github.com/vincentlauriat/serversmonitor/internal/hub/alerts"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/hub/ingest"
	"github.com/vincentlauriat/serversmonitor/internal/hub/server"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/hub/webdist"
)

type Hub struct {
	cfg     config.Config
	log     *slog.Logger
	st      *store.Store
	bus     *server.Broadcaster
	agents  *ingest.Handler
	machine *alerts.Machine
	handler http.Handler
}

func New(cfg config.Config, version string, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "serversmonitor.db"))
	if err != nil {
		return nil, err
	}
	if err := st.SeedDefaultRules(time.Now().UTC()); err != nil {
		st.Close()
		return nil, err
	}
	// Rebuild which alerts were firing before the restart, so a hub that comes
	// back up does not re-announce alerts nobody resolved.
	machine := alerts.NewMachine()
	last, err := st.LastEventPerKey()
	if err != nil {
		st.Close()
		return nil, err
	}
	var firing []alerts.Key
	for k, e := range last {
		if e.Kind == "fired" {
			firing = append(firing, alerts.Key{RuleID: k[0], HostID: k[1]})
		}
	}
	machine.Restore(firing)

	bus := server.NewBroadcaster()
	icfg := ingest.DefaultConfig()
	icfg.IntervalSec = st.SettingInt("agent_interval_sec", 10)
	agents := ingest.New(st, bus, icfg, log.With("component", "ingest"))
	h := &Hub{cfg: cfg, log: log, st: st, bus: bus, agents: agents, machine: machine}
	h.handler = server.New(server.Deps{Store: st, Agents: agents, Bus: bus, Static: webdist.Build(), InstallScript: deploy.InstallScript,
		Version: version, Log: log.With("component", "server"), Secure: cfg.Secure})
	return h, nil
}

func (h *Hub) Handler() http.Handler { return h.handler }

func (h *Hub) Close() error { return h.st.Close() }

func (h *Hub) interval() time.Duration {
	return time.Duration(h.st.SettingInt("agent_interval_sec", 10)) * time.Second
}

// Run executes the periodic jobs until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	fast := time.NewTicker(h.interval())
	minute := time.NewTicker(time.Minute)
	hour := time.NewTicker(time.Hour)
	defer fast.Stop()
	defer minute.Stop()
	defer hour.Stop()
	h.hourly() // catch up on restart
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fast.C:
			h.checkStale()
			fast.Reset(h.interval())
		case <-minute.C:
			h.evaluate()
		case <-hour.C:
			h.hourly()
		}
	}
}

// checkStale is where the hub, not the agent, decides a host is offline.
func (h *Hub) checkStale() {
	now := time.Now().UTC()
	stale, err := h.st.MarkStale(now, 3*h.interval())
	if err != nil {
		h.log.Error("mark stale", "err", err)
		return
	}
	for _, host := range stale {
		h.log.Warn("host went offline", "host", host.Name)
		h.bus.Publish("host", map[string]any{"id": host.ID})
	}
	if len(stale) > 0 {
		h.evaluate() // an offline transition should not wait for the minute tick
	}
}

func (h *Hub) evaluate() {
	now := time.Now().UTC()
	hosts, err := h.st.ListHosts()
	if err != nil {
		h.log.Error("list hosts", "err", err)
		return
	}
	rules, err := h.st.ListRules()
	if err != nil {
		h.log.Error("list rules", "err", err)
		return
	}
	latest := map[int64]store.SampleRow{}
	for _, host := range hosts {
		if row, err := h.st.LatestSample(host.ID); err == nil {
			latest[host.ID] = row
		}
	}
	events := alerts.Evaluate(h.machine, alerts.Input{Now: now, Interval: h.interval(), Hosts: hosts, Latest: latest, Rules: rules})
	for _, e := range events {
		if _, err := h.st.InsertAlertEvent(e); err != nil {
			h.log.Error("insert alert event", "err", err)
			continue
		}
		h.log.Info("alert", "kind", e.Kind, "metric", e.Metric, "host_id", e.HostID, "value", e.Value)
		h.bus.Publish("alert", map[string]any{"host_id": e.HostID, "metric": e.Metric, "kind": e.Kind, "value": e.Value})
	}
}

func (h *Hub) hourly() {
	now := time.Now().UTC()
	if err := h.st.Aggregate(now); err != nil {
		h.log.Error("aggregate", "err", err)
	}
	if err := h.st.Purge(now, h.st.RetentionFromSettings()); err != nil {
		h.log.Error("purge", "err", err)
	}
	if err := h.st.PurgeSessions(now); err != nil {
		h.log.Error("purge sessions", "err", err)
	}
}
