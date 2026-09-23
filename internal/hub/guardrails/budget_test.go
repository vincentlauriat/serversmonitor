package guardrails

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func day(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }

func keys(evs []store.GuardrailEvent) map[string]string {
	out := map[string]string{}
	for _, e := range evs {
		out[e.Subject+"|"+e.Rule+"|"+e.Detail] = e.Kind
	}
	return out
}

func TestThresholdsFireOnceEach(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 100, Spent: 85, AsOf: day(20), Settings: DefaultSettings(), Last: map[store.GuardrailKey]store.GuardrailEvent{}}
	got := keys(Budget(in))
	if got["budget|budget_threshold|80"] != "fired" || got["budget|budget_threshold|100"] != "" {
		t.Fatalf("got %v", got)
	}
	// Same input again, with the journal holding everything that fired the
	// first time (the threshold and the projection, which also crosses its
	// line at this Spent): nothing.
	in.Last[store.GuardrailKey{Subject: "budget", Rule: "budget_threshold", Detail: "80"}] = store.GuardrailEvent{Kind: "fired"}
	in.Last[store.GuardrailKey{Subject: "budget", Rule: "budget_projection"}] = store.GuardrailEvent{Kind: "fired"}
	if evs := Budget(in); len(evs) != 0 {
		t.Fatalf("re-fired: %v", keys(evs))
	}
}

func TestNewMonthResolvesEverything(t *testing.T) {
	last := map[store.GuardrailKey]store.GuardrailEvent{
		{Subject: "budget", Rule: "budget_threshold", Detail: "80"}:  {Kind: "fired"},
		{Subject: "budget", Rule: "budget_threshold", Detail: "100"}: {Kind: "fired"},
		{Subject: "budget", Rule: "budget_projection", Detail: ""}:   {Kind: "fired"},
		{Subject: "/s/vm1", Rule: "resource_share", Detail: ""}:      {Kind: "fired"},
	}
	in := BudgetInput{Now: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC), Budget: 100, Spent: 0, AsOf: time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), Settings: DefaultSettings(), Last: last}
	got := keys(Budget(in))
	for _, k := range []string{"budget|budget_threshold|80", "budget|budget_threshold|100", "budget|budget_projection|", "/s/vm1|resource_share|"} {
		if got[k] != "resolved" {
			t.Fatalf("%s = %q, want resolved (%v)", k, got[k], got)
		}
	}
}

func TestProjectionWaitsForFourBilledDaysAndHasHysteresis(t *testing.T) {
	if _, ok := Projection(day(4), day(4), 10); ok { // as_of the 4th → 3 billed days
		t.Fatal("3 billed days must not project")
	}
	p, ok := Projection(day(5), day(5), 10) // 4 billed days, 30-day month → 75
	if !ok || p < 74.9 || p > 75.1 {
		t.Fatalf("projection = %v %v", p, ok)
	}
	base := BudgetInput{Now: day(11), Budget: 100, AsOf: day(11), Settings: DefaultSettings()}
	// 10 billed days. Spent 34 → 102: inside the 5 % band, no fire.
	in := base
	in.Spent, in.Last = 34, map[store.GuardrailKey]store.GuardrailEvent{}
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "" {
		t.Fatalf("fired inside the band: %v", k)
	}
	// Spent 36 → 108 > 105: fires.
	in.Spent = 36
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "fired" {
		t.Fatalf("did not fire: %v", k)
	}
	// Firing, spent 33 → 99: still above 95, no resolve.
	in.Spent, in.Last = 33, map[store.GuardrailKey]store.GuardrailEvent{{Subject: "budget", Rule: "budget_projection", Detail: ""}: {Kind: "fired"}}
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "" {
		t.Fatalf("resolved inside the band: %v", k)
	}
	// Spent 31 → 93 < 95: resolves.
	in.Spent = 31
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "resolved" {
		t.Fatalf("did not resolve: %v", k)
	}
}

func TestResourceShareIsPerResourceAndCountsDeletedOnes(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 100, Spent: 60, AsOf: day(20), Settings: DefaultSettings(), Last: map[store.GuardrailKey]store.GuardrailEvent{},
		Rows: []CostRow{{ResourceID: "/s/vm1", Name: "vm1", Amount: 45}, {ResourceID: "/s/gone", Name: "gone", Amount: 31}, {ResourceID: "/s/site", Name: "site", Amount: 2}}}
	got := keys(Budget(in))
	if got["/s/vm1|resource_share|"] != "fired" || got["/s/gone|resource_share|"] != "fired" || got["/s/site|resource_share|"] != "" {
		t.Fatalf("got %v", got)
	}
	// The value carried is the share in percent.
	for _, e := range Budget(in) {
		if e.Subject == "/s/vm1" && (e.Value < 44.9 || e.Value > 45.1) {
			t.Fatalf("value = %v, want the share 45", e.Value)
		}
	}
}

func TestBudgetZeroDisablesAndResolves(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 0, Spent: 900, AsOf: day(20), Settings: DefaultSettings(),
		Last: map[store.GuardrailKey]store.GuardrailEvent{{Subject: "budget", Rule: "budget_threshold", Detail: "80"}: {Kind: "fired"}}}
	got := keys(Budget(in))
	if len(got) != 1 || got["budget|budget_threshold|80"] != "resolved" {
		t.Fatalf("got %v", got)
	}
}
