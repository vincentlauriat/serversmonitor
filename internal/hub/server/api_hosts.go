package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/alerts"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

// latestView mirrors store.SampleRow. Every nullable number stays a pointer so
// a metric that was not collected serializes as null, never as 0.
type latestView struct {
	At         time.Time    `json:"at"`
	CPU        *float64     `json:"cpu"`
	MemUsed    *int64       `json:"mem_used"`
	MemTotal   *int64       `json:"mem_total"`
	SwapUsed   *int64       `json:"swap_used"`
	SwapTotal  *int64       `json:"swap_total"`
	Load1      *float64     `json:"load1"`
	Load5      *float64     `json:"load5"`
	Load15     *float64     `json:"load15"`
	Uptime     *int64       `json:"uptime"`
	NetSentBps *float64     `json:"net_sent_bps"`
	NetRecvBps *float64     `json:"net_recv_bps"`
	Disks      []proto.Disk `json:"disks"`
	Temps      []proto.Temp `json:"temps"`
}

type hostView struct {
	ID           int64       `json:"id"`
	Name         string      `json:"name"`
	Status       string      `json:"status"`
	LastSeen     *time.Time  `json:"last_seen"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	Hostname     string      `json:"hostname"`
	AgentVersion string      `json:"agent_version"`
	Cores        int         `json:"cores"`
	MemTotal     int64       `json:"mem_total"`
	Muted        bool        `json:"muted"`
	Connected    bool        `json:"connected"`
	Latest       *latestView `json:"latest"`
	Firing       int         `json:"firing"`
}

func (s *server) hostView(h store.Host, connected map[int64]bool, firing map[int64]int) (hostView, error) {
	v := hostView{ID: h.ID, Name: h.Name, Status: h.Status, LastSeen: h.LastSeen, OS: h.OS, Arch: h.Arch, Hostname: h.Hostname,
		AgentVersion: h.AgentVersion, Cores: h.Cores, MemTotal: h.MemTotal, Muted: h.Muted, Connected: connected[h.ID], Firing: firing[h.ID]}
	row, err := s.Store.LatestSample(h.ID)
	if errors.Is(err, store.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.Latest = &latestView{At: row.At, CPU: row.CPU, MemUsed: row.MemUsed, MemTotal: row.MemTotal, SwapUsed: row.SwapUsed, SwapTotal: row.SwapTotal,
		Load1: row.Load1, Load5: row.Load5, Load15: row.Load15, Uptime: row.Uptime, NetSentBps: row.NetSentBps, NetRecvBps: row.NetRecvBps,
		Disks: row.Disks, Temps: row.Temps}
	return v, nil
}

func (s *server) connectedSet() map[int64]bool {
	out := map[int64]bool{}
	for _, id := range s.Agents.Connected() {
		out[id] = true
	}
	return out
}

// firingEvents returns the alerts that are firing *and still meaningful*.
//
// The event log is append-only and honest about what happened, so it keeps a
// "fired" row for two cases that can never produce a matching "resolved":
// a host muted while one of its rules was firing (the evaluator skips it
// entirely from then on), and a rule deleted while firing (nothing is left to
// resolve it). Filtering here rather than writing a synthetic "resolved" keeps
// the log truthful and the badge correct.
func (s *server) firingEvents() ([]store.AlertEvent, error) {
	last, err := s.Store.LastEventPerKey()
	if err != nil {
		return nil, err
	}
	hosts, err := s.Store.ListHosts()
	if err != nil {
		return nil, err
	}
	muted := map[int64]bool{}
	for _, h := range hosts {
		muted[h.ID] = h.Muted
	}
	rules, err := s.Store.ListRules()
	if err != nil {
		return nil, err
	}
	live := map[int64]bool{alerts.StatusRuleID: true} // the offline rule has no row
	for _, r := range rules {
		live[r.ID] = true
	}
	var out []store.AlertEvent
	for _, e := range last {
		if e.Kind != "fired" || muted[e.HostID] || !live[e.RuleID] {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *server) firingCounts() (map[int64]int, error) {
	firing, err := s.firingEvents()
	if err != nil {
		return nil, err
	}
	out := map[int64]int{}
	for _, e := range firing {
		out[e.HostID]++
	}
	return out, nil
}

func (s *server) handleListHosts(w http.ResponseWriter, r *http.Request, _ store.User) {
	hosts, err := s.Store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	firing, err := s.firingCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	connected := s.connectedSet()
	out := make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		v, err := s.hostView(h, connected, firing)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) installCommand(r *http.Request, token string) string {
	scheme, ws := "http", "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme, ws = "https", "wss"
	}
	return fmt.Sprintf("curl -fsSL %s://%s/install.sh | sudo sh -s -- --hub %s://%s --token %s", scheme, r.Host, ws, r.Host, token)
}

func (s *server) handleCreateHost(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	h, tok, err := s.Store.CreateHost(strings.TrimSpace(body.Name), s.Now())
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			writeErr(w, http.StatusConflict, "a host with this name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v, _ := s.hostView(h, nil, nil)
	s.Bus.Publish("hosts", nil)
	writeJSON(w, http.StatusCreated, map[string]any{"host": v, "token": tok, "install": s.installCommand(r, tok)})
}

func (s *server) handleGetHost(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	h, err := s.Store.Host(id)
	if err != nil {
		notFoundOr500(w, err)
		return
	}
	firing, err := s.firingCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v, err := s.hostView(h, s.connectedSet(), firing)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handlePatchHost(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Name  *string `json:"name"`
		Muted *bool   `json:"muted"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		if err := s.Store.RenameHost(id, name); err != nil {
			notFoundOr500(w, err)
			return
		}
	}
	if body.Muted != nil {
		if err := s.Store.SetMuted(id, *body.Muted); err != nil {
			notFoundOr500(w, err)
			return
		}
	}
	s.Bus.Publish("hosts", nil)
	s.handleGetHost(w, r, u)
}

func (s *server) handleDeleteHost(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteHost(id); err != nil {
		notFoundOr500(w, err)
		return
	}
	s.Bus.Publish("hosts", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRegenerateToken(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	tok, err := s.Store.RegenerateToken(id)
	if err != nil {
		notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "install": s.installCommand(r, tok)})
}

type pointView struct {
	At         time.Time    `json:"at"`
	CPU        *float64     `json:"cpu"`
	CPUMax     *float64     `json:"cpu_max"`
	MemUsed    *float64     `json:"mem_used"`
	MemTotal   *float64     `json:"mem_total"`
	SwapUsed   *float64     `json:"swap_used"`
	Load1      *float64     `json:"load1"`
	Load5      *float64     `json:"load5"`
	Load15     *float64     `json:"load15"`
	NetSentBps *float64     `json:"net_sent_bps"`
	NetRecvBps *float64     `json:"net_recv_bps"`
	Disks      []proto.Disk `json:"disks"`
	Temps      []proto.Temp `json:"temps"`
}

func (s *server) handleSeries(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	period := r.URL.Query().Get("period")
	if _, _, err := store.TableForPeriod(period); err != nil {
		writeErr(w, http.StatusBadRequest, "period must be one of 1h, 24h, 7d, 30d, 1y")
		return
	}
	pts, err := s.Store.Series(id, period, s.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]pointView, 0, len(pts))
	for _, p := range pts {
		out = append(out, pointView{At: p.At, CPU: p.CPU, CPUMax: p.CPUMax, MemUsed: p.MemUsed, MemTotal: p.MemTotal, SwapUsed: p.SwapUsed,
			Load1: p.Load1, Load5: p.Load5, Load15: p.Load15, NetSentBps: p.NetSentBps, NetRecvBps: p.NetRecvBps, Disks: p.Disks, Temps: p.Temps})
	}
	writeJSON(w, http.StatusOK, out)
}

type containerView struct {
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Status     string    `json:"status"`
	CPU        *float64  `json:"cpu"`
	MemUsed    *int64    `json:"mem_used"`
	NetSentBps *float64  `json:"net_sent_bps"`
	NetRecvBps *float64  `json:"net_recv_bps"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *server) handleContainers(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	cs, err := s.Store.Containers(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]containerView, 0, len(cs))
	for _, c := range cs {
		out = append(out, containerView{Name: c.Name, Image: c.Image, Status: c.Status, CPU: c.CPU, MemUsed: c.MemUsed,
			NetSentBps: c.NetSentBps, NetRecvBps: c.NetRecvBps, UpdatedAt: c.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}
