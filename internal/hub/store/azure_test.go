package store

import (
	"testing"
	"time"
)

func res(id, name string, state *string) AzureResource {
	return AzureResource{ID: id, Name: name, Type: "Microsoft.Web/sites",
		ResourceGroup: "rg", Location: "westeurope", State: state,
		Tags: map[string]string{"env": "dev"}}
}

func strp(s string) *string { return &s }

func TestAzureInventoryRoundTrip(t *testing.T) {
	s := openTest(t)
	in := []AzureResource{
		res("/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/a", "a", strp("Running")),
		res("/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/b", "b", nil),
	}
	if err := s.ReplaceAzureInventory([]string{"rg"}, in, t0); err != nil {
		t.Fatal(err)
	}
	out, err := s.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	byName := map[string]AzureResource{}
	for _, r := range out {
		byName[r.Name] = r
	}
	if got := byName["a"]; got.State == nil || *got.State != "Running" {
		t.Fatalf("a.state = %v", got.State)
	}
	if got := byName["b"]; got.State != nil {
		// A state nobody read is NULL, and must come back nil rather than "".
		t.Fatalf("b.state = %q, want nil", *got.State)
	}
	if byName["a"].Tags["env"] != "dev" {
		t.Fatalf("tags = %v", byName["a"].Tags)
	}
	if byName["a"].FirstSeen.IsZero() || byName["a"].LastSeen.IsZero() {
		t.Fatal("first_seen and last_seen must be set")
	}
	if byName["a"].DeletedAt != nil {
		t.Fatal("a resource that was just seen is not deleted")
	}
}

func TestAzureSweepSoftDeletesWhatVanished(t *testing.T) {
	s := openTest(t)
	a := res("/subs/rg/a", "a", strp("Running"))
	b := res("/subs/rg/b", "b", strp("Running"))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a, b}, t0)

	later := t0.Add(time.Hour)
	if err := s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, later); err != nil {
		t.Fatal(err)
	}
	out, _ := s.ListAzureResources()
	if len(out) != 2 {
		t.Fatalf("a vanished resource is soft-deleted, not removed: %d rows", len(out))
	}
	for _, r := range out {
		switch r.Name {
		case "a":
			if r.DeletedAt != nil {
				t.Fatal("a is still there")
			}
			if !r.LastSeen.Equal(later) {
				t.Fatalf("a.last_seen = %s", r.LastSeen)
			}
			if !r.FirstSeen.Equal(t0) {
				t.Fatalf("first_seen must not move on a re-sync: %s", r.FirstSeen)
			}
		case "b":
			if r.DeletedAt == nil {
				t.Fatal("b vanished and must carry deleted_at")
			}
			if r.Name != "b" {
				t.Fatal("a deleted resource keeps its name so its cost row can be labelled")
			}
		}
	}
}

func TestAzureResourceComingBackClearsDeletedAt(t *testing.T) {
	s := openTest(t)
	a := res("/subs/rg/a", "a", strp("Running"))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, t0)
	s.ReplaceAzureInventory([]string{"rg"}, nil, t0.Add(time.Hour))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, t0.Add(2*time.Hour))
	out, _ := s.ListAzureResources()
	if len(out) != 1 || out[0].DeletedAt != nil {
		t.Fatalf("a recreated resource is alive again: %+v", out)
	}
}

func TestAzureSweepOnlyTouchesTheGroupsItSynced(t *testing.T) {
	// Syncing one group must not mark another group's resources deleted.
	s := openTest(t)
	one := res("/subs/one/a", "a", nil)
	one.ResourceGroup = "rg-one"
	two := res("/subs/two/b", "b", nil)
	two.ResourceGroup = "rg-two"
	s.ReplaceAzureInventory([]string{"rg-one", "rg-two"}, []AzureResource{one, two}, t0)

	s.ReplaceAzureInventory([]string{"rg-one"}, []AzureResource{one}, t0.Add(time.Hour))
	out, _ := s.ListAzureResources()
	for _, r := range out {
		if r.Name == "b" && r.DeletedAt != nil {
			t.Fatal("rg-two was not synced, so its resources must be left alone")
		}
	}
}

func TestAzureCostsRoundTripAndUpsert(t *testing.T) {
	s := openTest(t)
	cs := []AzureCost{
		{ResourceID: "/subs/rg/a", Period: "2026-09", Amount: 1.5, Currency: "EUR", AsOf: t0},
		{ResourceID: "/subs/rg/gone", Period: "2026-09", Amount: 4.0, Currency: "EUR", AsOf: t0},
	}
	if err := s.UpsertAzureCosts(cs); err != nil {
		t.Fatal(err)
	}
	// A cost row for a resource that is not in the inventory must survive. The
	// absence of a foreign key is the point.
	out, err := s.ListAzureCosts("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d cost rows, want 2 including the orphan", len(out))
	}
	cs[0].Amount = 2.5
	cs[0].AsOf = t0.Add(time.Hour)
	if err := s.UpsertAzureCosts(cs[:1]); err != nil {
		t.Fatal(err)
	}
	out, _ = s.ListAzureCosts("2026-09")
	for _, c := range out {
		if c.ResourceID == "/subs/rg/a" && c.Amount != 2.5 {
			t.Fatalf("a second sync updates the amount, got %v", c.Amount)
		}
	}
	if got, _ := s.ListAzureCosts("2026-08"); len(got) != 0 {
		t.Fatal("periods are separate")
	}
}

func TestAzureSyncStateRoundTrip(t *testing.T) {
	s := openTest(t)
	if err := s.SetAzureSync(AzureSync{Scope: "inventory", OK: false,
		Message: "Forbidden: does not have authorization", StartedAt: t0, EndedAt: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	st, err := s.AzureSyncState()
	if err != nil {
		t.Fatal(err)
	}
	if st["inventory"].OK || st["inventory"].Message == "" {
		t.Fatalf("sync state = %+v", st["inventory"])
	}
	s.SetAzureSync(AzureSync{Scope: "inventory", OK: true, StartedAt: t0, EndedAt: t0})
	st, _ = s.AzureSyncState()
	if !st["inventory"].OK || st["inventory"].Message != "" {
		t.Fatalf("a success clears the message: %+v", st["inventory"])
	}
}

func TestPurgeAzureClearsEverything(t *testing.T) {
	s := openTest(t)
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{res("/a", "a", nil)}, t0)
	s.UpsertAzureCosts([]AzureCost{{ResourceID: "/a", Period: "2026-09", Amount: 1, Currency: "EUR", AsOf: t0}})
	if err := s.PurgeAzure(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "azure_resources"); n != 0 {
		t.Fatalf("%d resources left", n)
	}
	if n := count(t, s, "azure_costs"); n != 0 {
		t.Fatalf("%d cost rows left", n)
	}
}
