package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func byID(rs []store.AzureResource, id string) store.AzureResource {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return store.AzureResource{}
}

func mustList(t *testing.T, h *Hub) []store.AzureResource {
	t.Helper()
	rs, err := h.st.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestASuccessfulSyncJournalsGuardrailTransitionsOnce(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"} // unattached
	f.costs = map[string]float64{"site1": 45, "d1": 5}
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	h.ReloadAzure()

	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())

	last, _ := h.st.LastGuardrailEventPerKey()
	if last[store.GuardrailKey{Subject: f.id("d1"), Rule: "orphan"}].Kind != "fired" {
		t.Fatalf("orphan not journaled: %v", last)
	}
	if last[store.GuardrailKey{Subject: f.id("site1"), Rule: "resource_share"}].Kind != "fired" {
		t.Fatalf("share not journaled: %v", last)
	}
	n := len(last)
	// Same answers again: nothing new.
	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())
	evs, _ := h.st.ListGuardrailEvents(100)
	if len(evs) != n {
		t.Fatalf("journal grew from %d to %d on identical input", n, len(evs))
	}
	// The inventory carries the reason.
	rs, _ := h.st.ListAzureResources()
	if r := byID(rs, f.id("d1")); r.OrphanReason != "disk_unattached" || r.OrphanSince == nil {
		t.Fatalf("inventory: %+v", r)
	}
}

func TestAFailedOrphanReadLeavesTheInventoryGreenAndTheReasonsAlone(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"}
	h := azureHub(t, f)
	// A 500 is retried by the client itself; a fast Sleep keeps this test
	// from spending the real backoff schedule (1s, 5s, 25s).
	h.azureSleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	if r := byID(mustList(t, h), f.id("d1")); r.OrphanReason != "disk_unattached" {
		t.Fatalf("first sweep: %+v", r)
	}
	f.diskReadStatus = 500 // the typed read fails; the catalogue still answers
	h.syncAzureInventory(context.Background())
	st, _ := h.st.AzureSyncState()
	if !st["inventory"].OK {
		t.Fatalf("the inventory pass succeeded and must stay green: %+v", st["inventory"])
	}
	if st["orphans"].OK || st["orphans"].Message == "" {
		t.Fatalf("the orphan scope must carry the failure: %+v", st["orphans"])
	}
	if r := byID(mustList(t, h), f.id("d1")); r.OrphanReason != "disk_unattached" {
		t.Fatalf("a failed sweep must not clear yesterday's reasons: %+v", r)
	}
	if evs, _ := h.st.ListGuardrailEvents(10); len(evs) != 1 { // only the first fired event
		t.Fatalf("a failed sweep must not journal: %+v", evs)
	}
}

func TestAFailedSyncEvaluatesNothing(t *testing.T) {
	f := newAzureFake(t, "site1")
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	h.ReloadAzure()
	// Seed a cost row as if an earlier sync had already stored one. A fresh
	// store would make this check pass even with evaluateBudget wrongly
	// called on failure, since there would be nothing in it to fire on; this
	// row is what makes the check actually sensitive to that mistake.
	if err := h.st.UpsertAzureCosts([]store.AzureCost{{ResourceID: f.id("site1"),
		Period: currentPeriod(time.Now().UTC()), Amount: 95, Currency: "EUR", AsOf: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	f.failsFrom(1) // the token call succeeds, the first ARM call fails
	h.syncAzureCosts(context.Background())
	if evs, _ := h.st.ListGuardrailEvents(10); len(evs) != 0 {
		t.Fatalf("a failed sync must not journal: %+v", evs)
	}
}

// TestGuardrailTransitionsAreDelivered proves the wiring end to end: a fired
// transition reaches a channel, and every delivery row it wrote points back
// at the guardrail journal rather than the alert one. The replay path (a
// pending guardrail delivery left by a previous run) is covered separately
// by TestPendingGuardrailDeliveriesAreReplayedOnBoot in notify_test.go.
//
// The count of transitions that fire here is not asserted: evaluateBudget
// reads time.Now(), so budget_projection's own on/off band depends on how
// many days of the month are billed as of today, and would make an assertion
// pinned to a literal count flaky across the month. Asserting the message
// content and the delivery→journal link is what this test is for.
func TestGuardrailTransitionsAreDelivered(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.costs = map[string]float64{"site1": 95}
	h := azureHub(t, f)
	c := newCatcher(t)
	if err := notify.SaveConfig(h.st, notify.Config{
		Webhook: notify.WebhookConfig{Enabled: true, URL: c.srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	h.ReloadNotify()
	h.ReloadAzure()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.dispatch.Start(ctx)

	h.syncAzureCosts(ctx)

	p := c.await(t) // 95 >= the 80% threshold: budget_threshold(80) is always the first transition
	if !strings.Contains(p.Title, "Budget") || !strings.Contains(p.Title, "80") {
		t.Fatalf("title = %q", p.Title)
	}
	// The delivery rows are written before the dispatcher is even handed the
	// job (see notifyGuardrails), so they already exist once the sync call
	// above returned. Every delivery from a guardrail transition must point
	// at the guardrail journal, not the alert one.
	ds, err := h.st.ListDeliveries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) == 0 {
		t.Fatal("no deliveries recorded")
	}
	for _, d := range ds {
		if d.GuardrailEventID == nil {
			t.Fatalf("delivery %d has no guardrail event", d.ID)
		}
	}
}

func scheduleOn(t *testing.T, h *Hub, id string) {
	t.Helper()
	if err := h.st.UpsertAzureSchedule(store.AzureSchedule{ResourceID: id, OffWindows: guardrails.EncodeWindows(guardrails.EveningsAndWeekends), Enabled: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestABoundaryIssuesExactlyOneScheduledAction(t *testing.T) {
	f := newAzureFake(t, "site1")
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))

	wed2030 := time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC)
	h.applySchedules(wed2030)
	f.waitActions(t, 1)
	as, _ := h.st.ListAzureActions(5)
	if len(as) != 1 || as[0].Action != "stop" || as[0].Origin != "schedule" {
		t.Fatalf("actions = %+v", as)
	}
	// Ten minutes later: same boundary, nothing new.
	h.applySchedules(wed2030.Add(10 * time.Minute))
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 {
		t.Fatalf("replayed: %+v", as)
	}
	// Next morning: the end boundary → start.
	h.applySchedules(time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC))
	f.waitActions(t, 2)
	as, _ = h.st.ListAzureActions(5)
	if as[0].Action != "start" {
		t.Fatalf("morning action = %+v", as[0])
	}
}

func TestCatchUpIsBoundedToTwelveHours(t *testing.T) {
	// With evenings-and-weekends in UTC, the Friday 20:00 window merges with
	// the Saturday and Sunday all-day windows into one off interval running to
	// Monday 00:00. So the last boundary anywhere in the weekend is
	// Friday 2026-09-25 20:00 UTC, and this pair also exercises the merge.
	for _, tc := range []struct {
		name    string
		wake    time.Time
		applied bool
	}{
		{"11h59 after the boundary", time.Date(2026, 9, 26, 7, 59, 0, 0, time.UTC), true},
		{"12h01 after the boundary", time.Date(2026, 9, 26, 8, 1, 0, 0, time.UTC), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFake(t, "site1")
			h := azureHub(t, f)
			_ = h.st.SetSetting("azure_timezone", "UTC")
			h.ReloadAzure()
			h.syncAzureInventory(context.Background())
			scheduleOn(t, h, f.id("site1"))

			h.catchUpSchedules(tc.wake)
			if tc.applied {
				f.waitActions(t, 1)
				as, _ := h.st.ListAzureActions(5)
				if len(as) != 1 || as[0].Action != "stop" || as[0].Origin != "schedule" {
					t.Fatalf("expected one scheduled stop, got %+v", as)
				}
			} else {
				time.Sleep(100 * time.Millisecond)
				if as, _ := h.st.ListAzureActions(5); len(as) != 0 {
					t.Fatalf("a boundary older than twelve hours must not be applied: %+v", as)
				}
			}
			// Either way the boundary is marked, so the next tick does not
			// reconsider it.
			scs, _ := h.st.ListAzureSchedules()
			if scs[0].LastBoundary == nil || !scs[0].LastBoundary.Equal(time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)) {
				t.Fatalf("boundary not marked: %+v", scs[0].LastBoundary)
			}
		})
	}
}

// TestABoundaryThatProducesNoActionRowStillFires covers runSchedules' own
// call to journalScheduleFailure (no action row is ever created, so
// finishAction never runs) and, separately, journalScheduleFailure's own
// "already firing" guard. Those are two different properties: calling
// applySchedules again for the *same* boundary is refused by runSchedules'
// boundary dedup before journalScheduleFailure is even reached (last_boundary
// already advanced) — that alone would not prove the guard inside
// journalScheduleFailure does anything. So this test also crosses to the next
// boundary, where runSchedules does call through again with the resource
// still missing, and only the guard inside journalScheduleFailure keeps that
// from journalling a second "fired" event.
func TestABoundaryThatProducesNoActionRowStillFires(t *testing.T) {
	f := newAzureFake(t, "site1")
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	// A schedule on a resource the inventory does not have: StartActionFrom
	// refuses before writing any row, so finishAction never runs.
	if err := h.st.UpsertAzureSchedule(store.AzureSchedule{ResourceID: "/s/gone", OffWindows: guardrails.EncodeWindows(guardrails.EveningsAndWeekends), Enabled: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	waitFor(t, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: "/s/gone", Rule: "schedule_failed"}].Kind == "fired"
	}, "schedule_failed did not fire")
	// Same boundary, a minute later: runSchedules' own dedup refuses it
	// before journalScheduleFailure is reached at all.
	h.applySchedules(time.Date(2026, 9, 23, 20, 31, 0, 0, time.UTC))
	time.Sleep(100 * time.Millisecond)
	// The next morning's end boundary is a *different* boundary: runSchedules
	// calls through to journalScheduleFailure again, with the resource still
	// missing. Only the guard inside journalScheduleFailure itself can now
	// keep this from journalling a second "fired" event.
	h.applySchedules(time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC))
	time.Sleep(100 * time.Millisecond)
	n := 0
	evs, _ := h.st.ListGuardrailEvents(20)
	for _, e := range evs {
		if e.Rule == "schedule_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("schedule_failed journaled %d times", n)
	}
}

func TestAFailedScheduledActionFiresScheduleFailed(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.actionStatus = 500
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	waitFor(t, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: f.id("site1"), Rule: "schedule_failed"}].Kind == "fired"
	}, "schedule_failed did not fire")
	// The next boundary succeeds and resolves it.
	f.actionStatus = 0
	h.applySchedules(time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC))
	waitFor(t, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: f.id("site1"), Rule: "schedule_failed"}].Kind == "resolved"
	}, "schedule_failed did not resolve")
}

func TestAManualActionInFlightSkipsTheBoundary(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.holdAction = make(chan struct{})
	h := azureHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))
	if _, err := h.StartAction(f.id("site1"), azure.ActionRestart); err != nil {
		t.Fatal(err)
	}
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	close(f.holdAction)
	f.waitActions(t, 1)
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 || as[0].Origin != "user" {
		t.Fatalf("the boundary must be skipped, not queued: %+v", as)
	}
	// And it is not retried on the next tick either: last_boundary advanced.
	h.applySchedules(time.Date(2026, 9, 23, 20, 31, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 {
		t.Fatalf("retried: %+v", as)
	}
}

func TestDeleteOrphanNeedsTheNameAndADeletableKind(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"}
	f.ips = []string{"ip1"}
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())

	// Captured once: the fake's id() resolves a name against its current
	// disks/ips slices, and the delete below (and the "f.disks = nil" further
	// down, standing in for the fake dropping it) empties that list — a
	// second f.id("d1") after that point would resolve to "" and silently
	// break the final assertion's key rather than the code under test.
	diskID := f.id("d1")

	if err := h.DeleteOrphan(diskID, "d2"); !errors.Is(err, ErrWrongName) {
		t.Fatalf("wrong name: %v", err)
	}
	// azure.DeleteAny also refuses "ip" on its own, so errors.Is alone would
	// still pass with DeleteOrphan's own !deletable guard deleted — it would
	// just be DeleteAny's refusal reached over that inner defense-in-depth
	// line instead. What the hub-level guard buys, and what actually needs
	// checking, is that the refusal is an *azure.Refusal* — a client mistake
	// (400), never a bare error the HTTP layer would read as the hub's own
	// fault (500) — with the resource's name in it, not just its ARM id.
	if err := h.DeleteOrphan(f.id("ip1"), "ip1"); !errors.Is(err, azure.ErrUndeletableKind) || !azure.IsRefusal(err) {
		t.Fatalf("ip: %v", err)
	}
	if err := h.DeleteOrphan(diskID, "d1"); err != nil {
		t.Fatal(err)
	}
	f.waitDeletes(t, 1)
	// The next sweep no longer lists it (the fake dropped it), and the orphan event resolves.
	f.disks = nil
	h.syncAzureInventory(context.Background())
	last, _ := h.st.LastGuardrailEventPerKey()
	if last[store.GuardrailKey{Subject: diskID, Rule: "orphan"}].Kind != "resolved" {
		t.Fatalf("orphan not resolved after deletion: %v", last)
	}
}
