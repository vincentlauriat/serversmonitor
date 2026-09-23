package store

import (
	"testing"
	"time"
)

func TestGuardrailEventsJournalOnlyWhatIsInserted(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	e, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "fired", Value: 41.2, At: now})
	if err != nil || e.ID == 0 {
		t.Fatalf("insert: %v id=%d", err, e.ID)
	}
	if _, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "resolved", Value: 0, At: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "/subscriptions/x/disk1", Rule: "orphan", Kind: "fired", Value: 3, At: now}); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastGuardrailEventPerKey()
	if err != nil {
		t.Fatal(err)
	}
	if got := last[GuardrailKey{"budget", "budget_threshold", "80"}]; got.Kind != "resolved" {
		t.Fatalf("last for the 80 threshold = %q, want resolved", got.Kind)
	}
	if got := last[GuardrailKey{"/subscriptions/x/disk1", "orphan", ""}]; got.Kind != "fired" || got.Value != 3 {
		t.Fatalf("last orphan = %+v", got)
	}
	list, err := s.ListGuardrailEvents(2)
	if err != nil || len(list) != 2 || list[0].Kind != "resolved" && list[0].Rule != "orphan" {
		t.Fatalf("list newest first, 2 rows: %v %+v", err, list)
	}
}

func TestADeliveryPointsAtExactlyOneJournal(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	e, _ := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_projection", Kind: "fired", Value: 120, At: now})
	d, err := s.CreateGuardrailDelivery(e.ID, "smtp", now)
	if err != nil {
		t.Fatal(err)
	}
	if d.EventID != nil || d.GuardrailEventID == nil || *d.GuardrailEventID != e.ID {
		t.Fatalf("delivery = %+v", d)
	}
	got, err := s.DeliveryGuardrailEvent(d.ID)
	if err != nil || got.Rule != "budget_projection" {
		t.Fatalf("read back: %v %+v", err, got)
	}
	// An alert delivery still works the old way, and answers ErrNotFound on
	// the guardrail accessor rather than a zero event.
	h, _, _ := s.CreateHost("pi", now)
	ae, _ := s.InsertAlertEvent(AlertEvent{RuleID: 0, HostID: h.ID, Metric: "status", Kind: "fired", Value: 1, At: now})
	ad, err := s.CreateDelivery(ae.ID, "smtp", now)
	if err != nil || ad.EventID == nil || *ad.EventID != ae.ID || ad.GuardrailEventID != nil {
		t.Fatalf("alert delivery = %+v %v", ad, err)
	}
	if _, err := s.DeliveryGuardrailEvent(ad.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound for an alert delivery, got %v", err)
	}
	// Pending deliveries of both kinds come back together, in id order.
	pend, _ := s.PendingDeliveries()
	if len(pend) != 2 || pend[0].ID != d.ID {
		t.Fatalf("pending = %+v", pend)
	}
}

// TestADeliveryWithNoEventIsRefused is the mutation check for the CHECK
// constraint on deliveries: a row with neither event_id nor
// guardrail_event_id set must be impossible, not merely unused.
func TestADeliveryWithNoEventIsRefused(t *testing.T) {
	s := openTest(t)
	if _, err := s.db.Exec(`INSERT INTO deliveries (channel,state,created_at,updated_at) VALUES ('smtp','pending','x','x')`); err == nil {
		t.Fatal("a delivery with neither event_id nor guardrail_event_id set must be refused")
	}
}
