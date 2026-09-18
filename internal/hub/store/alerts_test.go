package store

import (
	"testing"
	"time"
)

func TestRulesCRUDAndSeed(t *testing.T) {
	s := openTest(t)
	if err := s.SeedDefaultRules(t0); err != nil {
		t.Fatal(err)
	}
	rules, _ := s.ListRules()
	if len(rules) != 4 {
		t.Fatalf("seed = %d rules", len(rules))
	}
	if err := s.SeedDefaultRules(t0); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ListRules(); len(again) != 4 {
		t.Fatal("seed must be a no-op when rules exist")
	}
	h, _, _ := s.CreateHost("a", t0)
	r, err := s.CreateRule(Rule{HostID: &h.ID, Metric: "disk", Threshold: 95, Duration: time.Minute}, t0)
	if err != nil || r.ID == 0 || r.HostID == nil || *r.HostID != h.ID {
		t.Fatalf("create = %+v %v", r, err)
	}
	r.Threshold = 97
	if err := s.UpdateRule(r); err != nil {
		t.Fatal(err)
	}
	rules, _ = s.ListRules()
	var found bool
	for _, x := range rules {
		if x.ID == r.ID && x.Threshold == 97 && x.Duration == time.Minute {
			found = true
		}
	}
	if !found {
		t.Fatalf("update lost: %+v", rules)
	}
	if err := s.DeleteRule(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule(r.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v", err)
	}
}

func TestAlertEventsAndLastPerKey(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: h.ID, Metric: "cpu", Kind: "fired", Value: 95, At: t0})
	s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: h.ID, Metric: "cpu", Kind: "resolved", Value: 40, At: t0.Add(time.Minute)})
	s.InsertAlertEvent(AlertEvent{RuleID: 0, HostID: h.ID, Metric: "status", Kind: "fired", Value: 0, At: t0.Add(2 * time.Minute)})
	evs, err := s.ListAlertEvents(nil, 10, 0)
	if err != nil || len(evs) != 3 || evs[0].Metric != "status" {
		t.Fatalf("list = %+v %v", evs, err)
	}
	page, _ := s.ListAlertEvents(&h.ID, 1, 1)
	if len(page) != 1 || page[0].Kind != "resolved" {
		t.Fatalf("page = %+v", page)
	}
	last, err := s.LastEventPerKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || last[[2]int64{1, h.ID}].Kind != "resolved" || last[[2]int64{0, h.ID}].Kind != "fired" {
		t.Fatalf("last = %+v", last)
	}
}

func TestLastEventPerKeySeparatesHosts(t *testing.T) {
	s := openTest(t)
	a, _, _ := s.CreateHost("a", t0)
	b, _, _ := s.CreateHost("b", t0)
	s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: a.ID, Metric: "cpu", Kind: "fired", Value: 95, At: t0})
	s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: b.ID, Metric: "cpu", Kind: "resolved", Value: 10, At: t0.Add(time.Minute)})
	last, _ := s.LastEventPerKey()
	if last[[2]int64{1, a.ID}].Kind != "fired" || last[[2]int64{1, b.ID}].Kind != "resolved" {
		t.Fatalf("one host's event must not shadow another's: %+v", last)
	}
}
