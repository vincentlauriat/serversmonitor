package hub

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// catchUpWindow bounds how old a missed boundary may be and still be applied
// at startup. Older than this, the hub says nothing rather than acting on a
// stale intention — see catchUpSchedules.
const catchUpWindow = 12 * time.Hour

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

// DeleteOrphan is lot 5's delete-by-name generalised to an ARM id and type:
// the second and last destructive call this hub makes, and it has the same
// shape as the first — the name typed back, or nothing happens. There is no
// ownership check here, unlike DeleteProvision: an orphan is by definition
// something nobody is using, whoever created it, and the typed name is the
// consent that stands in for one.
//
// It runs synchronously rather than firing a goroutine the way
// DeleteProvision does. A disk, a NIC or a plan deletes in seconds, well
// inside the handler's budget, and a modal that says "deleting…" and then
// "gone" is what the person expects — no polling required on the page side.
func (h *Hub) DeleteOrphan(resourceID, confirmName string) error {
	_, client, ok := h.azureReady()
	if !ok {
		return ErrAzureOff
	}
	r, err := h.st.ActionableAzureResource(azure.NormalizeID(resourceID))
	if err != nil {
		return err
	}
	// Fail closed independent of the equality check below: a blank stored
	// name and a blank confirmation are equal strings, but "nothing typed"
	// must never satisfy "nothing to confirm".
	confirm := strings.TrimSpace(confirmName)
	if confirm == "" || r.Name == "" || confirm != r.Name {
		return &azure.Refusal{Err: fmt.Errorf("%w: type %q to confirm", ErrWrongName, r.Name)}
	}
	kind, deletable := azure.KindOf(r.Type)
	if !deletable {
		return &azure.Refusal{Err: fmt.Errorf("%w: %s (%s)", azure.ErrUndeletableKind, r.Name, r.Type)}
	}
	armID := r.ARMID
	if armID == "" {
		armID = r.ID // written before arm_id existed; the next sync fills it in
	}
	ctx, cancel := context.WithTimeout(h.baseCtx(), 10*time.Minute)
	defer cancel()
	if err := azure.DeleteAny(ctx, client, armID, kind); err != nil {
		return err
	}
	h.kickAzure() // the next sweep drops the row and resolves the orphan event
	return nil
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

// journalScheduleFailure fires schedule_failed for resourceID, unless it is
// already firing. Two call sites, for the two ways a boundary can fail: here
// from runSchedules, for a boundary that produced no action row at all (the
// resource left the inventory, or its type stopped being actionable); and
// from finishAction, for a scheduled action whose row existed but failed.
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

// journalScheduleResolved is journalScheduleFailure's resolve half: a
// scheduled action for resourceID has now succeeded. Called only from
// finishAction — runSchedules never has a success to report, since a boundary
// it could apply goes through StartActionFrom and finishAction from there.
func (h *Hub) journalScheduleResolved(resourceID string) {
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	if last[store.GuardrailKey{Subject: resourceID, Rule: "schedule_failed"}].Kind != "fired" {
		return
	}
	h.journalGuardrails([]store.GuardrailEvent{{Subject: resourceID, Rule: "schedule_failed", Kind: "resolved", At: time.Now().UTC()}})
}

// applySchedules acts on boundaries, never on states: a resource switched on
// by hand inside its off window stays on until the next boundary — the hub
// never fights a person. Called every minute.
func (h *Hub) applySchedules(now time.Time) { h.runSchedules(now, 0) }

// catchUpSchedules is applySchedules with an age limit, for the boundary the
// hub may have slept through while it was down. Called once at startup.
func (h *Hub) catchUpSchedules(now time.Time) { h.runSchedules(now, catchUpWindow) }

// runSchedules is applySchedules and catchUpSchedules' shared body. maxAge
// disables catch-up (0 = no limit, i.e. the ordinary minute tick); a positive
// maxAge refuses a boundary older than that, marking it seen without acting
// on it — a boundary missed by hours is a stale intention, not one to replay
// now.
func (h *Hub) runSchedules(now time.Time, maxAge time.Duration) {
	if _, _, ok := h.azureReady(); !ok {
		return
	}
	scs, err := h.st.ListAzureSchedules()
	if err != nil {
		h.log.Error("schedules: list", "err", err)
		return
	}
	loc := h.GuardrailSettings().Location()
	for _, sc := range scs {
		if !sc.Enabled {
			continue
		}
		ws, err := guardrails.ParseWindows(sc.OffWindows)
		if err != nil || len(ws) == 0 {
			continue
		}
		b, off, ok := guardrails.LastBoundary(ws, loc, now)
		// ok=false is LastBoundary's overloaded "nothing" — but len(ws) == 0
		// was already handled above, so here it can only mean "no boundary at
		// or before now within the lookback": the ordinary state of a
		// resource in the middle of a long on-period, not a fault. Leave it
		// alone — not disabled, not journalled, not marked.
		if !ok || (sc.LastBoundary != nil && !b.After(*sc.LastBoundary)) {
			continue
		}
		if maxAge > 0 && now.Sub(b) > maxAge {
			h.log.Warn("schedules: missed boundary, too old to apply", "resource", sc.ResourceID, "boundary", b, "age", now.Sub(b))
			if err := h.st.MarkScheduleBoundary(sc.ResourceID, b, now); err != nil {
				h.log.Error("schedules: mark boundary", "err", err)
			}
			continue
		}
		// Advance first: a crash after this line loses the action, never
		// duplicates it — lot 4's rule for interrupted actions (see
		// interruptActions in azure_action.go), applied here to a boundary
		// instead of an in-flight call.
		if err := h.st.MarkScheduleBoundary(sc.ResourceID, b, now); err != nil {
			h.log.Error("schedules: mark boundary", "err", err)
			continue
		}
		action := azure.ActionStart
		if off {
			action = azure.ActionStop
		}
		// StartActionFrom normalizes the id itself; normalize here too so the
		// guardrail key below matches the one finishAction uses (r.ID, always
		// normalized) rather than whatever casing this row happens to hold.
		id := azure.NormalizeID(sc.ResourceID)
		if _, err := h.StartActionFrom(id, action, "schedule"); err != nil {
			if errors.Is(err, store.ErrActionInFlight) {
				h.log.Warn("schedules: a manual action is running, boundary skipped", "resource", sc.ResourceID, "boundary", b)
				continue
			}
			// No action row exists, so finishAction will never run for this
			// boundary and would never journal it. A resource that left the
			// inventory, or whose type stopped being actionable, would
			// otherwise stay as it is with nothing said. Spec §5 promises an
			// event here.
			h.log.Warn("schedules: action refused", "resource", sc.ResourceID, "action", action, "err", err)
			h.journalScheduleFailure(id, err.Error())
		}
	}
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
