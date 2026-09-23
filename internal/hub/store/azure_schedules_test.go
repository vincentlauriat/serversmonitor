package store

import (
	"testing"
	"time"
)

func TestSchedulesRoundTripAndBoundaryAdvances(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	if err := s.UpsertAzureSchedule(AzureSchedule{ResourceID: "/subscriptions/x/sites/a", OffWindows: `[{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]`, Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAzureSchedules()
	if err != nil || len(list) != 1 || list[0].LastBoundary != nil || !list[0].Enabled {
		t.Fatalf("list = %+v %v", list, err)
	}
	b := now.Add(-time.Hour)
	if err := s.MarkScheduleBoundary("/subscriptions/x/sites/a", b, now); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListAzureSchedules()
	if list[0].LastBoundary == nil || !list[0].LastBoundary.Equal(b) || list[0].LastAppliedAt == nil {
		t.Fatalf("boundary not recorded: %+v", list[0])
	}
	// Upsert keeps the boundary: editing the windows must not replay the last action.
	if err := s.UpsertAzureSchedule(AzureSchedule{ResourceID: "/subscriptions/x/sites/a", OffWindows: `[]`, Enabled: false}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListAzureSchedules()
	if list[0].Enabled || list[0].LastBoundary == nil {
		t.Fatalf("upsert lost state: %+v", list[0])
	}
	if err := s.DeleteAzureSchedule("/subscriptions/x/sites/a"); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListAzureSchedules(); len(list) != 0 {
		t.Fatal("not deleted")
	}
}

func TestOrphanSinceSurvivesASweepWithTheSameReason(t *testing.T) {
	s := openTest(t)
	t0 := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	rows := []AzureResource{{ID: "/s/disk1", ARMID: "/S/disk1", Name: "disk1", Type: "Microsoft.Compute/disks", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}
	if err := s.ReplaceAzureInventory([]string{"rg"}, rows, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAzureOrphans(map[string]string{"/s/disk1": "disk_unattached"}, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAzureResources()
	if r := byID(got, "/s/disk1"); r.OrphanReason != "disk_unattached" || r.OrphanSince == nil || !r.OrphanSince.Equal(t0) {
		t.Fatalf("first detection: %+v", r)
	}
	// A later sweep with the same reason keeps the original since.
	if err := s.ReplaceAzureInventory([]string{"rg"}, rows, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAzureOrphans(map[string]string{"/s/disk1": "disk_unattached"}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListAzureResources()
	if r := byID(got, "/s/disk1"); !r.OrphanSince.Equal(t0) {
		t.Fatalf("since moved: %+v", r)
	}
	// A different reason resets it; an absent id clears it.
	if err := s.SetAzureOrphans(map[string]string{"/s/site1": "unverified"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListAzureResources()
	if r := byID(got, "/s/disk1"); r.OrphanReason != "" || r.OrphanSince != nil {
		t.Fatalf("not cleared: %+v", r)
	}
	if r := byID(got, "/s/site1"); r.OrphanReason != "unverified" || !r.OrphanSince.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("not set: %+v", r)
	}
}

func byID(rs []AzureResource, id string) AzureResource {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return AzureResource{}
}

func TestActionOriginIsStoredAndListed(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	if err := s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/s/site1", ResourceName: "site1", Action: "stop", Origin: "schedule", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	as, _ := s.ListAzureActions(5)
	if len(as) != 1 || as[0].Origin != "schedule" {
		t.Fatalf("origin lost: %+v", as)
	}
	// Empty origin is written as "user", never as "".
	_ = s.FinishAzureAction(as[0].ID, "succeeded", "", nil, now)
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/s/site1", ResourceName: "site1", Action: "start", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	as, _ = s.ListAzureActions(5)
	if as[0].Origin != "user" {
		t.Fatalf("default origin = %q", as[0].Origin)
	}
}
