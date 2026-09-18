package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

var validMetrics = map[string]bool{"cpu": true, "memory": true, "disk": true, "load": true, "temperature": true, "bandwidth": true}

type ruleView struct {
	ID          int64   `json:"id"`
	HostID      *int64  `json:"host_id"`
	Metric      string  `json:"metric"`
	Threshold   float64 `json:"threshold"`
	DurationSec int64   `json:"duration_sec"`
}

func toRuleView(r store.Rule) ruleView {
	return ruleView{ID: r.ID, HostID: r.HostID, Metric: r.Metric, Threshold: r.Threshold, DurationSec: int64(r.Duration / time.Second)}
}

type eventView struct {
	ID       int64     `json:"id"`
	RuleID   int64     `json:"rule_id"`
	HostID   int64     `json:"host_id"`
	HostName string    `json:"host_name"`
	Metric   string    `json:"metric"`
	Kind     string    `json:"kind"`
	Value    float64   `json:"value"`
	At       time.Time `json:"at"`
}

func (s *server) hostNames() (map[int64]string, error) {
	hosts, err := s.Store.ListHosts()
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, h := range hosts {
		out[h.ID] = h.Name
	}
	return out, nil
}

func toEventViews(evs []store.AlertEvent, names map[int64]string) []eventView {
	out := make([]eventView, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventView{ID: e.ID, RuleID: e.RuleID, HostID: e.HostID, HostName: names[e.HostID],
			Metric: e.Metric, Kind: e.Kind, Value: e.Value, At: e.At})
	}
	return out
}

func (s *server) handleAlerts(w http.ResponseWriter, r *http.Request, _ store.User) {
	last, err := s.Store.LastEventPerKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	names, err := s.hostNames()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var firing []store.AlertEvent
	for _, e := range last {
		if e.Kind == "fired" {
			firing = append(firing, e)
		}
	}
	rules, err := s.Store.ListRules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rv := make([]ruleView, 0, len(rules))
	for _, rule := range rules {
		rv = append(rv, toRuleView(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"firing": toEventViews(firing, names), "rules": rv})
}

func (s *server) handleAlertEvents(w http.ResponseWriter, r *http.Request, _ store.User) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	var hostID *int64
	if v := r.URL.Query().Get("host"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad host")
			return
		}
		hostID = &id
	}
	const per = 50
	evs, err := s.Store.ListAlertEvents(hostID, per, (page-1)*per)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	names, err := s.hostNames()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toEventViews(evs, names))
}

func (s *server) readRule(w http.ResponseWriter, r *http.Request) (store.Rule, bool) {
	var body ruleView
	if err := readJSON(w, r, &body); err != nil || !validMetrics[body.Metric] || body.DurationSec < 0 {
		return store.Rule{}, false
	}
	return store.Rule{HostID: body.HostID, Metric: body.Metric, Threshold: body.Threshold,
		Duration: time.Duration(body.DurationSec) * time.Second}, true
}

func (s *server) handleCreateRule(w http.ResponseWriter, r *http.Request, _ store.User) {
	rule, ok := s.readRule(w, r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "metric must be one of cpu, memory, disk, load, temperature, bandwidth")
		return
	}
	created, err := s.Store.CreateRule(rule, s.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toRuleView(created))
}

func (s *server) handleUpdateRule(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rule, ok := s.readRule(w, r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid rule")
		return
	}
	rule.ID = id
	if err := s.Store.UpdateRule(rule); err != nil {
		notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuleView(rule))
}

func (s *server) handleDeleteRule(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteRule(id); err != nil {
		notFoundOr500(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
