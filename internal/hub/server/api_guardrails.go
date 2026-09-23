package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// --- GET /api/v1/azure/guardrails -----------------------------------------

type thresholdView struct {
	Pct    int     `json:"pct"`
	Line   float64 `json:"line"`
	Firing bool    `json:"firing"`
}

type shareView struct {
	ResourceID string  `json:"resource_id"`
	Name       string  `json:"name"`
	Amount     float64 `json:"amount"`
	SharePct   float64 `json:"share_pct"`
	Firing     bool    `json:"firing"`
}

type orphanView struct {
	ResourceID string   `json:"resource_id"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Reason     string   `json:"reason"`
	Since      *string  `json:"since"`
	Cost       *float64 `json:"cost"`
	Currency   string   `json:"currency,omitempty"`
	Deletable  bool     `json:"deletable"`
}

type guardrailEventView struct {
	Subject string  `json:"subject"`
	Name    string  `json:"name"`
	Rule    string  `json:"rule"`
	Detail  string  `json:"detail"`
	Kind    string  `json:"kind"`
	Value   float64 `json:"value"`
	At      string  `json:"at"`
}

type guardrailsView struct {
	Budget     float64              `json:"budget"`
	Spent      float64              `json:"spent"`
	Currencies []string             `json:"currencies"`
	Projection *float64             `json:"projection"` // null before day 4
	DaysBilled int                  `json:"days_billed"`
	Thresholds []thresholdView      `json:"thresholds"`
	Shares     []shareView          `json:"shares"`
	Orphans    []orphanView         `json:"orphans"`
	Events     []guardrailEventView `json:"events"` // last 20
	Timezone   string               `json:"timezone"`
}

// daysBilled mirrors guardrails' own private daysBilled: how many days of the
// current month Cost Management has actually billed as of asOf. Unexported
// there, so the view — which shows the figure even inside the four-day dead
// zone Projection refuses to guess in — recomputes it rather than importing
// what the package deliberately does not export.
func daysBilled(now, asOf time.Time) int {
	now, asOf = now.UTC(), asOf.UTC()
	if asOf.IsZero() || asOf.Year() != now.Year() || asOf.Month() != now.Month() {
		return 0
	}
	return asOf.Day() - 1
}

func (s *server) handleGetGuardrails(w http.ResponseWriter, r *http.Request, _ store.User) {
	now := s.Now().UTC()
	period := now.Format("2006-01")
	cfg := azure.LoadConfig(s.Store)
	set := s.Azure.GuardrailSettings()

	spent, currencies, byID, asOf, err := costSummary(s.Store, period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resources, err := s.Store.ListAzureResources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := make(map[string]string, len(resources))
	for _, res := range resources {
		names[res.ID] = res.Name
	}
	last, err := s.Store.LastGuardrailEventPerKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	v := guardrailsView{Budget: cfg.BudgetMonthly, Spent: spent, Currencies: currencies,
		DaysBilled: daysBilled(now, asOf), Timezone: set.Timezone,
		Thresholds: make([]thresholdView, 0, len(set.Thresholds)),
		Shares:     make([]shareView, 0), Orphans: make([]orphanView, 0), Events: make([]guardrailEventView, 0)}
	if p, ok := guardrails.Projection(now, asOf, spent); ok {
		v.Projection = &p
	}

	for _, pct := range set.Thresholds {
		firing := last[store.GuardrailKey{Subject: "budget", Rule: "budget_threshold", Detail: strconv.Itoa(pct)}].Kind == "fired"
		v.Thresholds = append(v.Thresholds, thresholdView{Pct: pct, Line: cfg.BudgetMonthly * float64(pct) / 100, Firing: firing})
	}

	// Shares: every resource above the line, budget rules' own definition —
	// see guardrails.Budget. A budget of 0 has no line, so nothing is above it.
	if cfg.BudgetMonthly > 0 {
		shareLine := cfg.BudgetMonthly * float64(set.ResourceShare) / 100
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			c := byID[id]
			if c.Amount <= shareLine {
				continue
			}
			name := names[id]
			if name == "" {
				name = azure.LastSegment(id)
			}
			firing := last[store.GuardrailKey{Subject: id, Rule: "resource_share"}].Kind == "fired"
			v.Shares = append(v.Shares, shareView{ResourceID: id, Name: name, Amount: c.Amount,
				SharePct: c.Amount / cfg.BudgetMonthly * 100, Firing: firing})
		}
	}

	for _, res := range resources {
		if res.OrphanReason == "" {
			continue
		}
		// A 403 on the typed read is not "healthy": unverified means the hub
		// neither affirms nor denies, and it must not offer a delete button
		// for something it never confirmed is safe to remove.
		_, deletable := azure.KindOf(res.Type)
		if res.OrphanReason == azure.OrphanUnverified {
			deletable = false
		}
		ov := orphanView{ResourceID: res.ID, Name: res.Name, Type: res.Type, Reason: res.OrphanReason, Deletable: deletable}
		if res.OrphanSince != nil {
			since := res.OrphanSince.UTC().Format(time.RFC3339)
			ov.Since = &since
		}
		if c, ok := byID[res.ID]; ok {
			amount := c.Amount
			ov.Cost, ov.Currency = &amount, c.Currency
		}
		v.Orphans = append(v.Orphans, ov)
	}

	events, err := s.Store.ListGuardrailEvents(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, e := range events {
		name := ""
		if e.Subject != "budget" {
			name = names[e.Subject]
			if name == "" {
				name = azure.LastSegment(e.Subject)
			}
		}
		v.Events = append(v.Events, guardrailEventView{Subject: e.Subject, Name: name, Rule: e.Rule,
			Detail: e.Detail, Kind: e.Kind, Value: e.Value, At: e.At.UTC().Format(time.RFC3339)})
	}

	writeJSON(w, http.StatusOK, v)
}

// --- GET/PUT /api/v1/azure/guardrails/settings ----------------------------

// guardrailSettingsView is both what the browser reads and what it sends
// back: unlike the Azure credentials, none of these four settings is a
// secret, so there is nothing to shape differently between the two.
type guardrailSettingsView struct {
	Thresholds      []int  `json:"thresholds"`
	ResourceShare   int    `json:"resource_share_pct"`
	HubVMSilentDays int    `json:"hub_vm_silent_days"`
	Timezone        string `json:"timezone"`
}

func (s *server) handleGetGuardrailSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := guardrails.LoadSettings(s.Store)
	writeJSON(w, http.StatusOK, guardrailSettingsView{Thresholds: c.Thresholds, ResourceShare: c.ResourceShare,
		HubVMSilentDays: c.HubVMSilentDays, Timezone: c.Timezone})
}

func (s *server) handlePutGuardrailSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in guardrailSettingsView
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	c := guardrails.Settings{Thresholds: in.Thresholds, ResourceShare: in.ResourceShare,
		HubVMSilentDays: in.HubVMSilentDays, Timezone: in.Timezone}
	// Validated before writing, like every other Azure setting: a partial
	// save would leave the evaluator reading a rule nobody asked for.
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := guardrails.SaveSettings(s.Store, c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET/PUT /api/v1/azure/schedules --------------------------------------

type scheduleView struct {
	ResourceID   string              `json:"resource_id"`
	Name         string              `json:"name"`
	OffWindows   []guardrails.Window `json:"off_windows"`
	Enabled      bool                `json:"enabled"`
	LastBoundary *string             `json:"last_boundary"`
	OffNow       bool                `json:"off_now"`
}

func (s *server) handleGetSchedules(w http.ResponseWriter, r *http.Request, _ store.User) {
	scs, err := s.Store.ListAzureSchedules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resources, err := s.Store.ListAzureResources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := make(map[string]string, len(resources))
	for _, res := range resources {
		names[res.ID] = res.Name
	}
	loc := s.Azure.GuardrailSettings().Location()
	now := s.Now().UTC()

	out := make([]scheduleView, 0, len(scs))
	for _, sc := range scs {
		ws, err := guardrails.ParseWindows(sc.OffWindows)
		if err != nil {
			// Written through handlePutSchedule, which validates before
			// storing: a parse failure here is corruption, not a bad request.
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("stored schedule for %s: %v", sc.ResourceID, err))
			return
		}
		name := names[sc.ResourceID]
		if name == "" {
			name = azure.LastSegment(sc.ResourceID)
		}
		v := scheduleView{ResourceID: sc.ResourceID, Name: name, OffWindows: ws, Enabled: sc.Enabled,
			OffNow: sc.Enabled && guardrails.Off(ws, loc, now)}
		if v.OffWindows == nil {
			v.OffWindows = []guardrails.Window{}
		}
		if sc.LastBoundary != nil {
			b := sc.LastBoundary.UTC().Format(time.RFC3339)
			v.LastBoundary = &b
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

type scheduleInput struct {
	ResourceID string              `json:"resource_id"`
	OffWindows []guardrails.Window `json:"off_windows"`
	Enabled    bool                `json:"enabled"`
}

// handlePutSchedule stores a resource's off-hours windows. The id travels in
// the body, never the path — an ARM id is mostly slashes, the lot 4 rule
// every action route already follows.
//
// resource_id is normalized here, exactly as runSchedules normalizes it
// before using it as a guardrail Subject (see guardrails_hub.go): this is the
// first real writer of azure_schedules, and a write keyed differently from
// the read would key a fired schedule_failed event differently from its
// resolved pair, so the alert would never close.
func (s *server) handlePutSchedule(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in scheduleInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	id := azure.NormalizeID(in.ResourceID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "resource_id is required")
		return
	}
	res, err := s.Store.ActionableAzureResource(id)
	if err != nil {
		if errors.Is(err, store.ErrNoSuchResource) {
			writeErr(w, http.StatusNotFound, "no such resource")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !azure.Supports(res.Type, azure.ActionStop) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a schedule needs a type the hub can stop; %s is not one", res.Type))
		return
	}
	// Round-tripped through ParseWindows/EncodeWindows rather than stored as
	// decoded: that is the one place window validation lives (day range,
	// HH:MM format, sorted days), and a second copy of those rules here would
	// be the second convention the brief warns against.
	raw, err := json.Marshal(in.OffWindows)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid off_windows")
		return
	}
	ws, err := guardrails.ParseWindows(string(raw))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpsertAzureSchedule(store.AzureSchedule{ResourceID: id,
		OffWindows: guardrails.EncodeWindows(ws), Enabled: in.Enabled}, s.Now().UTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type scheduleDeleteInput struct {
	ResourceID string `json:"resource_id"`
}

func (s *server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in scheduleDeleteInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Same normalization as the write: a differently-cased id would silently
	// delete nothing (DELETE ... WHERE resource_id = ? affects zero rows, no
	// error) rather than the row a person is looking at on the page.
	if err := s.Store.DeleteAzureSchedule(azure.NormalizeID(in.ResourceID)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/azure/orphans/delete ------------------------------------

type orphanDeleteInput struct {
	ResourceID  string `json:"resource_id"`
	ConfirmName string `json:"confirm_name"`
}

// handleDeleteOrphan does not normalize or otherwise interpret the id or the
// confirmation itself: DeleteOrphan normalizes the id internally (see
// guardrails_hub.go) and compares the confirmation verbatim, the same
// division of labour as the lot 5 VM delete route.
func (s *server) handleDeleteOrphan(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in orphanDeleteInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	err := s.Azure.DeleteOrphan(in.ResourceID, in.ConfirmName)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNoSuchResource):
		writeErr(w, http.StatusNotFound, "no such resource")
	case errors.Is(err, azure.ErrNotConfigured):
		writeErr(w, http.StatusBadRequest, "Azure is not configured")
	case azure.IsRefusal(err):
		// A wrong name or an undeletable kind: the person can act on it, so
		// the message — Azure's own words, wrapped, never replaced — passes
		// through rather than flattening to "bad request".
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		// Whatever is left came back from the ARM call itself (DeleteAny /
		// Await), not from this hub's own bookkeeping — the same distinction
		// handleTestAzure draws with the same status.
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}
