package store

import (
	"errors"
	"testing"
	"time"
)

const nicARM = "/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/networkInterfaces/vm1-nic"
const vmARM = "/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Compute/virtualMachines/vm1"

func TestAProvisionRecordsEachResourceAsItLands(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	hostID := int64(7)

	id, err := s.StartAzureProvision("vm1", &hostID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAzureProvisionRunning(id); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ arm, kind string }{{nicARM, "nic"}, {vmARM, "vm"}} {
		if err := s.RecordAzureProvisionResource(id, r.arm, r.kind, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishAzureProvision(id, "succeeded", "", now); err != nil {
		t.Fatal(err)
	}

	ps, err := s.ListAzureProvisions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("provisions = %d, want 1", len(ps))
	}
	p := ps[0]
	if p.Status != "succeeded" || p.HostID == nil || *p.HostID != 7 || p.FinishedAt == nil {
		t.Fatalf("provision = %+v", p)
	}
	if len(p.Resources) != 2 || p.Resources[0].Kind != "nic" || p.Resources[1].ARMID != vmARM {
		t.Fatalf("resources = %+v", p.Resources)
	}
	// ARM's own casing survived: the delete URL is built from it.
	if p.Resources[1].ARMID != vmARM {
		t.Fatalf("arm_id = %q, want %q", p.Resources[1].ARMID, vmARM)
	}
}

func TestASecondProvisionOfTheSameNameIsRefusedByTheDatabase(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	if _, err := s.StartAzureProvision("vm1", nil, now); err != nil {
		t.Fatal(err)
	}
	_, err := s.StartAzureProvision("vm1", nil, now)
	if !errors.Is(err, ErrProvisionInFlight) {
		t.Fatalf("err = %v, want ErrProvisionInFlight", err)
	}
}

func TestAFinishedProvisionDoesNotBlockANewOneWithTheSameName(t *testing.T) {
	// The index is partial on purpose. Without the WHERE clause a VM could be
	// created once and never again under that name, even after being deleted.
	s := openTest(t)
	now := time.Now().UTC()
	id, err := s.StartAzureProvision("vm1", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAzureProvision(id, "failed", "boom", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAzureProvision("vm1", nil, now); err != nil {
		t.Fatalf("a second attempt after a failure must be allowed: %v", err)
	}
}

func TestAnInterruptedProvisionKeepsItsResources(t *testing.T) {
	// The whole point of the table. A hub that died mid-run may have created a
	// NIC that is costing money; forgetting it would be the silent failure.
	s := openTest(t)
	now := time.Now().UTC()
	id, err := s.StartAzureProvision("vm1", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAzureProvisionResource(id, nicARM, "nic", now); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptAzureProvisions(now)
	if err != nil || n != 1 {
		t.Fatalf("interrupted %d, err %v", n, err)
	}
	ps, _ := s.ListAzureProvisions(10)
	if ps[0].Status != "interrupted" {
		t.Fatalf("status = %s", ps[0].Status)
	}
	if len(ps[0].Resources) != 1 || ps[0].Resources[0].ARMID != nicARM {
		t.Fatalf("the leftover must survive the interruption: %+v", ps[0].Resources)
	}
	if ps[0].Resources[0].DeletedAt != nil {
		t.Fatal("nothing was deleted, and the record must not claim otherwise")
	}
}

func TestMarkingAResourceDeletedTwiceIsNotAnError(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	id, _ := s.StartAzureProvision("vm1", nil, now)
	if err := s.RecordAzureProvisionResource(id, nicARM, "nic", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkAzureProvisionResourceDeleted(nicARM, now); err != nil {
			t.Fatalf("press %d: %v", i+1, err)
		}
	}
	ps, _ := s.ListAzureProvisions(10)
	if ps[0].Resources[0].DeletedAt == nil {
		t.Fatal("deleted_at was not set")
	}
}

func TestAResourceNobodyRecordedCannotBeFound(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.AzureProvisionResourceByARMID(vmARM); !errors.Is(err, ErrNoSuchProvision) {
		t.Fatalf("err = %v, want ErrNoSuchProvision", err)
	}
}

func TestNoProvisionsIsAnEmptyListNotNil(t *testing.T) {
	s := openTest(t)
	ps, err := s.ListAzureProvisions(10)
	if err != nil {
		t.Fatal(err)
	}
	if ps == nil {
		t.Fatal("want an empty slice, got nil — it renders as null over JSON")
	}
}
