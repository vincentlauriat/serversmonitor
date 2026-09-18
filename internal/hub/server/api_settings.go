package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type settingsView struct {
	AgentIntervalSec  int    `json:"agent_interval_sec"`
	RetentionRawHours int    `json:"retention_raw_hours"`
	Retention10mDays  int    `json:"retention_10m_days"`
	Retention1hDays   int    `json:"retention_1h_days"`
	Version           string `json:"version,omitempty"`
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	writeJSON(w, http.StatusOK, settingsView{
		AgentIntervalSec:  s.Store.SettingInt("agent_interval_sec", 10),
		RetentionRawHours: s.Store.SettingInt("retention_raw_hours", 24),
		Retention10mDays:  s.Store.SettingInt("retention_10m_days", 30),
		Retention1hDays:   s.Store.SettingInt("retention_1h_days", 365),
		Version:           s.Version,
	})
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body settingsView
	if err := readJSON(w, r, &body); err != nil || body.AgentIntervalSec < 1 || body.AgentIntervalSec > 3600 ||
		body.RetentionRawHours < 1 || body.Retention10mDays < 1 || body.Retention1hDays < 1 {
		writeErr(w, http.StatusBadRequest, "interval must be 1..3600 s and retentions at least 1")
		return
	}
	prev := s.Store.SettingInt("agent_interval_sec", 10)
	for k, v := range map[string]int{"agent_interval_sec": body.AgentIntervalSec, "retention_raw_hours": body.RetentionRawHours,
		"retention_10m_days": body.Retention10mDays, "retention_1h_days": body.Retention1hDays} {
		if err := s.Store.SetSetting(k, strconv.Itoa(v)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if prev != body.AgentIntervalSec {
		s.Agents.Reconfigure(body.AgentIntervalSec)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams bus events as Server-Sent Events.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request, _ store.User) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ch, cancel := s.Bus.Subscribe()
	defer cancel()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}
