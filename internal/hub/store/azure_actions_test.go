package store

import (
	"errors"
	"testing"
	"time"
)

func seedResource(t *testing.T, s *Store, id, armID, name string) {
	t.Helper()
	r := AzureResource{ID: id, ARMID: armID, Name: name, Type: "microsoft.web/sites",
		ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}
	if err := s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{r}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestArmIDSurvivesTheUpsert(t *testing.T) {
	// The action URL is built from ARM's own casing. NormalizeID lowercased the
	// join key, so the original has to be carried alongside it or the first
	// real action after the credential unblocks is a guess.
	s := openTest(t)
	const lower = "/subscriptions/s/resourcegroups/rg/providers/microsoft.web/sites/app"
	const arm = "/subscriptions/S/resourceGroups/rg/providers/Microsoft.Web/sites/app"
	seedResource(t, s, lower, arm, "app")
	rs, err := s.ListAzureResources()
	if err != nil || len(rs) != 1 {
		t.Fatalf("list = %d, %v", len(rs), err)
	}
	if rs[0].ID != lower {
		t.Fatalf("id = %q, want the lowercased join key", rs[0].ID)
	}
	if rs[0].ARMID != arm {
		t.Fatalf("arm_id = %q, want %q", rs[0].ARMID, arm)
	}
}

func TestStartWritesPendingBeforeAnythingElse(t *testing.T) {
	s := openTest(t)
	seedResource(t, s, "/x", "/X", "app")
	now := time.Now().UTC().Truncate(time.Second)
	id, err := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app",
		Action: "stop", RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	as, err := s.ListAzureActions(10)
	if err != nil || len(as) != 1 {
		t.Fatalf("list = %d, %v", len(as), err)
	}
	a := as[0]
	if a.ID != id || a.Status != "pending" || a.FinishedAt != nil {
		t.Fatalf("got %+v, want a pending row with no finished_at", a)
	}
	if !a.RequestedAt.Equal(now) {
		t.Fatalf("requested_at = %v, want %v", a.RequestedAt, now)
	}
}

func TestASecondActionOnTheSameResourceIsRefused(t *testing.T) {
	s := openTest(t)
	seedResource(t, s, "/x", "/X", "app")
	seedResource(t, s, "/y", "/Y", "other")
	now := time.Now().UTC()
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app", Action: "stop", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, err := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app", Action: "start", RequestedAt: now})
	if !errors.Is(err, ErrActionInFlight) {
		t.Fatalf("second action on the same resource: err = %v, want ErrActionInFlight", err)
	}
	// And the guard must be per resource, not global: the partial index is easy
	// to write in a way that locks the whole table.
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/y", ResourceName: "other", Action: "stop", RequestedAt: now}); err != nil {
		t.Fatalf("another resource must still be actionable: %v", err)
	}
}

func TestAFinishedActionFreesTheResource(t *testing.T) {
	s := openTest(t)
	seedResource(t, s, "/x", "/X", "app")
	now := time.Now().UTC()
	id, err := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app", Action: "stop", RequestedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	stopped := "Stopped"
	if err := s.FinishAzureAction(id, "succeeded", "", &stopped, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app", Action: "start", RequestedAt: now}); err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
	as, _ := s.ListAzureActions(10)
	var done AzureAction
	for _, a := range as {
		if a.ID == id {
			done = a
		}
	}
	if done.Status != "succeeded" || done.StateAfter == nil || *done.StateAfter != "Stopped" || done.FinishedAt == nil {
		t.Fatalf("finished row = %+v", done)
	}
}

func TestInterruptedOnStartup(t *testing.T) {
	// An action in flight when the hub died is never replayed: re-firing a stop
	// could stop a resource restarted by hand in the meantime. The honest
	// record is "nobody knows", and the next sync says what actually happened.
	s := openTest(t)
	seedResource(t, s, "/x", "/X", "app")
	seedResource(t, s, "/y", "/Y", "other")
	seedResource(t, s, "/z", "/Z", "third")
	now := time.Now().UTC()
	pending, _ := s.StartAzureAction(AzureAction{ResourceID: "/x", ResourceName: "app", Action: "stop", RequestedAt: now})
	running, _ := s.StartAzureAction(AzureAction{ResourceID: "/y", ResourceName: "other", Action: "stop", RequestedAt: now})
	if err := s.MarkAzureActionRunning(running); err != nil {
		t.Fatal(err)
	}
	done, _ := s.StartAzureAction(AzureAction{ResourceID: "/z", ResourceName: "third", Action: "start", RequestedAt: now})
	if err := s.FinishAzureAction(done, "succeeded", "", nil, now); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptAzureActions(now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("interrupted %d, want 2", n)
	}
	byID := map[int64]AzureAction{}
	as, _ := s.ListAzureActions(10)
	for _, a := range as {
		byID[a.ID] = a
	}
	for _, id := range []int64{pending, running} {
		if byID[id].Status != "interrupted" {
			t.Fatalf("action %d = %s, want interrupted", id, byID[id].Status)
		}
		if byID[id].StateAfter != nil {
			t.Fatalf("an interrupted action knows nothing about the state after")
		}
	}
	if byID[done].Status != "succeeded" {
		t.Fatalf("a finished action must not be touched, got %s", byID[done].Status)
	}
}

func TestListIsMostRecentFirstAndLimited(t *testing.T) {
	s := openTest(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		seedResource(t, s, "/"+id, "/"+id, id)
		aid, err := s.StartAzureAction(AzureAction{ResourceID: "/" + id, ResourceName: id,
			Action: "stop", RequestedAt: base.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		s.FinishAzureAction(aid, "succeeded", "", nil, base)
	}
	as, err := s.ListAzureActions(3)
	if err != nil || len(as) != 3 {
		t.Fatalf("list = %d, %v", len(as), err)
	}
	if as[0].ResourceName != "e" || as[2].ResourceName != "c" {
		t.Fatalf("order = %s %s %s, want e d c", as[0].ResourceName, as[1].ResourceName, as[2].ResourceName)
	}
}

func TestADeletedResourceIsNotActionable(t *testing.T) {
	// ListAzureResources returns soft-deleted rows on purpose: the page shows
	// them. Acting on one is another matter — Azure no longer has it.
	s := openTest(t)
	now := time.Now().UTC()
	seedResource(t, s, "/x", "/X", "app")
	// A later sweep that no longer sees it marks it deleted.
	if err := s.ReplaceAzureInventory([]string{"rg"}, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActionableAzureResource("/x"); !errors.Is(err, ErrNoSuchResource) {
		t.Fatalf("err = %v, want ErrNoSuchResource for a deleted resource", err)
	}
	seedResource(t, s, "/live", "/Live", "live")
	got, err := s.ActionableAzureResource("/live")
	if err != nil {
		t.Fatalf("a live resource must be actionable: %v", err)
	}
	if got.ARMID != "/Live" {
		t.Fatalf("arm id = %q", got.ARMID)
	}
}
