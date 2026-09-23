package hub

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestGuardrailTransitionsAreDeliveredAndReplayed proves the wiring end to
// end: a fired transition reaches a channel, and every delivery row it wrote
// points back at the guardrail journal rather than the alert one.
//
// The count of transitions that fire here is not asserted: evaluateBudget
// reads time.Now(), so budget_projection's own on/off band depends on how
// many days of the month are billed as of today, and would make an assertion
// pinned to a literal count flaky across the month. Asserting the message
// content and the delivery→journal link is what this test is for.
func TestGuardrailTransitionsAreDeliveredAndReplayed(t *testing.T) {
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
