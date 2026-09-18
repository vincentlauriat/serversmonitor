package store

import (
	"testing"
	"time"
)

func seedEvent(t *testing.T, s *Store, hostID int64, kind string, at time.Time) AlertEvent {
	t.Helper()
	e, err := s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: hostID, Metric: "cpu", Kind: kind, Value: 95, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeliveryLifecycle(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	d, err := s.CreateDelivery(e.ID, "smtp", t0)
	if err != nil || d.ID == 0 || d.State != "pending" || d.Attempts != 0 {
		t.Fatalf("create = %+v %v", d, err)
	}
	pending, _ := s.PendingDeliveries()
	if len(pending) != 1 || pending[0].ID != d.ID {
		t.Fatalf("pending = %+v", pending)
	}
	if err := s.MarkDeliverySent(d.ID, t0.Add(time.Second), 2); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Delivery(d.ID)
	if got.State != "sent" || got.Attempts != 2 || got.LastError != "" {
		t.Fatalf("sent = %+v", got)
	}
	if p, _ := s.PendingDeliveries(); len(p) != 0 {
		t.Fatal("a sent delivery is no longer pending")
	}
}

func TestDeliveryFailureKeepsTheReason(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	d, _ := s.CreateDelivery(e.ID, "webhook", t0)
	if err := s.MarkDeliveryFailed(d.ID, t0.Add(time.Minute), 4, "503 Service Unavailable"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Delivery(d.ID)
	if got.State != "failed" || got.Attempts != 4 || got.LastError != "503 Service Unavailable" {
		t.Fatalf("failed = %+v", got)
	}
}

func TestDeliveryEventCarriesItsHost(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "resolved", t0)
	d, _ := s.CreateDelivery(e.ID, "teams", t0)
	ev, host, err := s.DeliveryEvent(d.ID)
	if err != nil || ev.ID != e.ID || ev.Kind != "resolved" || host.Name != "pi" {
		t.Fatalf("event = %+v host = %+v err = %v", ev, host, err)
	}
	if _, _, err := s.DeliveryEvent(9999); err != ErrNotFound {
		t.Fatalf("missing delivery = %v", err)
	}
}

func TestDeliveriesCascadeWithTheHost(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	s.CreateDelivery(e.ID, "smtp", t0)
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("fixture did not land: %d", n)
	}
	if err := s.DeleteHost(h.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 0 {
		t.Fatalf("deleting a host must cascade to deliveries, %d left", n)
	}
}

func TestChannelHealthKeepsTheLatestPerChannel(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e1 := seedEvent(t, s, h.ID, "fired", t0)
	e2 := seedEvent(t, s, h.ID, "resolved", t0.Add(time.Minute))
	d1, _ := s.CreateDelivery(e1.ID, "smtp", t0)
	s.MarkDeliveryFailed(d1.ID, t0, 4, "boom")
	d2, _ := s.CreateDelivery(e2.ID, "smtp", t0.Add(time.Minute))
	s.MarkDeliverySent(d2.ID, t0.Add(time.Minute), 1)
	d3, _ := s.CreateDelivery(e1.ID, "webhook", t0)
	s.MarkDeliveryFailed(d3.ID, t0, 4, "dns")
	health, err := s.ChannelHealth()
	if err != nil {
		t.Fatal(err)
	}
	if health["smtp"].State != "sent" {
		t.Fatalf("smtp health must be the latest, got %+v", health["smtp"])
	}
	if health["webhook"].LastError != "dns" {
		t.Fatalf("webhook health = %+v", health["webhook"])
	}
	if _, ok := health["teams"]; ok {
		t.Fatal("a channel that never delivered has no health row")
	}
}

func TestPurgeDeliveries(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	old, _ := s.CreateDelivery(e.ID, "smtp", t0)
	s.MarkDeliverySent(old.ID, t0, 1)
	recent, _ := s.CreateDelivery(e.ID, "webhook", t0.Add(48*time.Hour))
	s.MarkDeliverySent(recent.ID, t0.Add(48*time.Hour), 1)
	if err := s.PurgeDeliveries(t0.Add(49*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("purge left %d rows, want 1", n)
	}
}

func TestPurgeKeepsPendingWhateverItsAge(t *testing.T) {
	// A delivery still pending is work not yet done; age must not delete it,
	// or a hub that was offline for a day comes back with nothing to send.
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	s.CreateDelivery(e.ID, "smtp", t0)
	if err := s.PurgeDeliveries(t0.Add(365*24*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("a pending delivery must survive the purge, %d left", n)
	}
}

func TestDeliveriesReferenceTheirEvent(t *testing.T) {
	s := openTest(t)
	var ref string
	if err := s.db.QueryRow(`SELECT "table" FROM pragma_foreign_key_list('deliveries')`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != "alert_events" {
		t.Fatalf("deliveries must reference alert_events, got %q", ref)
	}
}
