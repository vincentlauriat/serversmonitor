package hub

import (
	"context"
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func (h *Hub) GuardrailSettings() guardrails.Settings { return guardrails.LoadSettings(h.st) }

// evaluateBudget runs after a successful cost sync and journals only what
// changed. It reads the same rows the page reads, so the two never disagree.
func (h *Hub) evaluateBudget() {
	cfg := azure.LoadConfig(h.st)
	now := time.Now().UTC()
	costs, err := h.st.ListAzureCosts(currentPeriod(now))
	if err != nil {
		h.log.Error("guardrails: list costs", "err", err)
		return
	}
	names := h.resourceNames()
	var in guardrails.BudgetInput
	in.Now, in.Budget, in.Settings = now, cfg.BudgetMonthly, h.GuardrailSettings()
	currencies := map[string]bool{}
	for _, c := range costs {
		in.Spent += c.Amount
		currencies[c.Currency] = true
		if c.AsOf.After(in.AsOf) {
			in.AsOf = c.AsOf
		}
		in.Rows = append(in.Rows, guardrails.CostRow{ResourceID: c.ResourceID, Name: names[c.ResourceID], Amount: c.Amount})
	}
	in.Currencies = len(currencies)
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	in.Last = last
	h.journalGuardrails(guardrails.Budget(in))
}

// evaluateOrphans runs inside a successful inventory sync, with the freshly
// read resources: the typed reads happen before the rows are stored, so a
// read that fails fails the whole sweep (a partial inventory is worse than none).
func (h *Hub) evaluateOrphans(ctx context.Context, client *azure.Client, rs []azure.Resource) (map[string]string, error) {
	reasons, err := azure.OrphanReasons(ctx, client, rs)
	if err != nil {
		return nil, err
	}
	set := h.GuardrailSettings()
	now := time.Now().UTC()
	for _, r := range rs {
		if r.Tags[azure.CreatedByTag] != azure.CreatedByValue {
			continue
		}
		hostID, ok := parseHostID(r.Tags[azure.HostIDTag])
		if !ok {
			continue
		}
		host, err := h.st.Host(hostID)
		if err != nil {
			continue
		}
		if guardrails.HubVMSilent(host.Status, host.LastSeen, now, set.HubVMSilentDays) {
			reasons[r.ID] = azure.OrphanHubVMSilent
		}
	}
	return reasons, nil
}

func (h *Hub) journalOrphans(reasons map[string]string) {
	now := time.Now().UTC()
	if err := h.st.SetAzureOrphans(reasons, now); err != nil {
		h.log.Error("guardrails: store orphans", "err", err)
		return
	}
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	h.journalGuardrails(guardrails.Orphans(guardrails.OrphanInput{Now: now, Reasons: reasons, Names: h.resourceNames(), Last: last}))
}

// journalGuardrails is the single writer of the guardrail journal. Every line
// it writes is published and handed to the dispatcher, success and failure
// alike (lot 3's silent failure is the defect this guards against).
func (h *Hub) journalGuardrails(evs []store.GuardrailEvent) {
	var saved []store.GuardrailEvent
	for _, e := range evs {
		stored, err := h.st.InsertGuardrailEvent(e)
		if err != nil {
			h.log.Error("guardrails: insert", "err", err)
			continue
		}
		h.log.Info("guardrail", "rule", e.Rule, "subject", e.Subject, "kind", e.Kind, "value", e.Value)
		saved = append(saved, stored)
	}
	if len(saved) > 0 {
		h.notifyGuardrails(saved)
		h.bus.Publish("azure", map[string]any{"scope": "guardrails", "ok": true})
	}
}

// journalScheduleFailure fires schedule_failed for a boundary that produced no
// action row. Kept beside runSchedules because that is the only caller; the
// resolve side lives in finishAction, where a later success is observed.
func (h *Hub) journalScheduleFailure(resourceID, msg string) {
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	if last[store.GuardrailKey{Subject: resourceID, Rule: "schedule_failed"}].Kind == "fired" {
		return
	}
	h.log.Warn("schedules: boundary not applied", "resource", resourceID, "err", msg)
	h.journalGuardrails([]store.GuardrailEvent{{Subject: resourceID, Rule: "schedule_failed", Kind: "fired", At: time.Now().UTC()}})
}

func (h *Hub) resourceNames() map[string]string {
	out := map[string]string{}
	rs, err := h.st.ListAzureResources()
	if err != nil {
		return out
	}
	for _, r := range rs {
		out[r.ID] = r.Name
	}
	return out
}

func parseHostID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}
