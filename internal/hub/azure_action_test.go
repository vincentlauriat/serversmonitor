package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// show prints a state that may be unread, without leaking a pointer address
// into the failure message.
func show(p *string) string {
	if p == nil {
		return "<unread>"
	}
	return *p
}

const siteA = "/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/a"

// waitAction polls the action row until it is finished, or fails the test.
func waitAction(t *testing.T, h *Hub, id int64) store.AzureAction {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		as, err := h.st.ListAzureActions(50)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.ID == id && a.FinishedAt != nil {
				return a
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("action %d never finished", id)
	return store.AzureAction{}
}

func TestStartActionRefusesAnUnknownResource(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())

	_, err := h.StartAction(siteA+"-nope", azure.ActionStop)
	if !errors.Is(err, store.ErrNoSuchResource) {
		t.Fatalf("err = %v, want ErrNoSuchResource", err)
	}
	// And nothing was sent to Azure. The hub does not act on an id it cannot
	// show; the browser is not a security boundary.
	if calls := f.actionCalls(); len(calls) != 0 {
		t.Fatalf("Azure was called %v", calls)
	}
}

func TestStartActionRefusesWhenAzureIsOff(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	if err := azure.SaveConfig(h.st, azure.Config{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	h.ReloadAzure()

	if _, err := h.StartAction(siteA, azure.ActionStop); !errors.Is(err, ErrAzureOff) {
		t.Fatalf("err = %v, want ErrAzureOff", err)
	}
	if as, _ := h.st.ListAzureActions(10); len(as) != 0 {
		t.Fatalf("no row must be written, got %d", len(as))
	}
}

func TestStartActionRefusesAnUnactionableType(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	// Store the same resource as a server farm, which has no actions.
	if err := h.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{{
		ID: siteA, ARMID: siteA, Name: "a", Type: "microsoft.web/serverfarms",
		ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartAction(siteA, azure.ActionStop); !errors.Is(err, ErrNotActionable) {
		t.Fatalf("err = %v, want ErrNotActionable", err)
	}
	if calls := f.actionCalls(); len(calls) != 0 {
		t.Fatalf("Azure was called %v", calls)
	}
}

func TestSuccessfulActionReadsTheStateBack(t *testing.T) {
	f := newAzureFake(t, "a")
	f.stateAfter = "Stopped"
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())

	id, err := h.StartAction(siteA, azure.ActionStop)
	if err != nil {
		t.Fatal(err)
	}
	a := waitAction(t, h, id)
	if a.Status != "succeeded" {
		t.Fatalf("status = %s, error = %s", a.Status, a.Error)
	}
	if a.StateAfter == nil || *a.StateAfter != "Stopped" {
		t.Fatalf("state_after = %v", a.StateAfter)
	}
	calls := f.actionCalls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/stop") {
		t.Fatalf("Azure calls = %v", calls)
	}
	rs, _ := h.st.ListAzureResources()
	if rs[0].State == nil || *rs[0].State != "Stopped" {
		t.Fatalf("the inventory must carry the state that was read, got %v", rs[0].State)
	}
}

func TestTheStateIsNeverInferred(t *testing.T) {
	// Azure has not caught up: the stop succeeded but the read still says
	// Running. The hub stores what it read, not what it asked for. This test
	// fails the day someone writes the optimistic shortcut.
	f := newAzureFake(t, "a")
	f.stateAfter = "Running"
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())

	id, _ := h.StartAction(siteA, azure.ActionStop)
	a := waitAction(t, h, id)
	if a.Status != "succeeded" {
		t.Fatalf("status = %s", a.Status)
	}
	if a.StateAfter == nil || *a.StateAfter != "Running" {
		t.Fatalf("state_after = %s, want Running — what Azure said", show(a.StateAfter))
	}
	rs, _ := h.st.ListAzureResources()
	if rs[0].State == nil || *rs[0].State != "Running" {
		t.Fatalf("stored state = %v, want Running", rs[0].State)
	}
}

func TestAFailedReadBackLeavesTheStateAlone(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	f.mu.Lock()
	f.readStatus = 500
	f.mu.Unlock()

	id, _ := h.StartAction(siteA, azure.ActionStop)
	a := waitAction(t, h, id)
	if a.Status != "succeeded" {
		t.Fatalf("the action itself worked; status = %s", a.Status)
	}
	if a.StateAfter != nil {
		t.Fatalf("state_after = %v, want NULL — the read failed", *a.StateAfter)
	}
	rs, _ := h.st.ListAzureResources()
	if rs[0].State == nil || *rs[0].State != "Running" {
		t.Fatalf("the previous state must stand, got %v", rs[0].State)
	}
}

func TestAFailedActionKeepsAzuresMessage(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	f.mu.Lock()
	f.actionStatus = 403
	f.mu.Unlock()

	id, _ := h.StartAction(siteA, azure.ActionStop)
	a := waitAction(t, h, id)
	if a.Status != "failed" {
		t.Fatalf("status = %s", a.Status)
	}
	if !strings.Contains(a.Error, "Website Contributor") {
		t.Fatalf("Azure's own message must survive: %q", a.Error)
	}
}

func TestBothOutcomesArePublished(t *testing.T) {
	// The defect found by hand in lot 3 was a failure nobody published. This is
	// that lesson, as a test.
	for _, tc := range []struct {
		name   string
		status int
	}{{"success", 0}, {"failure", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFake(t, "a")
			h := azureHub(t, f)
			h.syncAzureInventory(context.Background())
			f.mu.Lock()
			f.actionStatus = tc.status
			f.mu.Unlock()
			ch, stop := h.bus.Subscribe()
			defer stop()

			id, err := h.StartAction(siteA, azure.ActionStop)
			if err != nil {
				t.Fatal(err)
			}
			waitAction(t, h, id)
			deadline := time.After(2 * time.Second)
			for {
				select {
				case ev := <-ch:
					if ev.Type == "azure_action" {
						return
					}
				case <-deadline:
					t.Fatal("no azure_action event was published")
				}
			}
		})
	}
}

func TestASecondActionWhileOneRunsIsRefused(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	f.mu.Lock()
	f.holdAction = make(chan struct{})
	f.mu.Unlock()
	defer close(f.holdAction)

	if _, err := h.StartAction(siteA, azure.ActionStop); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartAction(siteA, azure.ActionStart); !errors.Is(err, store.ErrActionInFlight) {
		t.Fatalf("err = %v, want ErrActionInFlight", err)
	}
}

func TestInterruptedActionsAreNotReplayed(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	now := time.Now().UTC()
	id, err := h.st.StartAzureAction(store.AzureAction{ResourceID: siteA, ResourceName: "a",
		Action: "stop", RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.MarkAzureActionRunning(id); err != nil {
		t.Fatal(err)
	}

	h.interruptActions() // what startup does

	as, _ := h.st.ListAzureActions(10)
	if as[0].Status != "interrupted" {
		t.Fatalf("status = %s, want interrupted", as[0].Status)
	}
	if calls := f.actionCalls(); len(calls) != 0 {
		t.Fatalf("nothing must be replayed, Azure saw %v", calls)
	}
}
