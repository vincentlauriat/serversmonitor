package guardrails

import (
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type CostRow struct {
	ResourceID string
	Name       string
	Amount     float64
}

type BudgetInput struct {
	Now        time.Time
	Budget     float64
	Spent      float64
	Currencies int
	AsOf       time.Time
	Rows       []CostRow
	Settings   Settings
	Last       map[store.GuardrailKey]store.GuardrailEvent
}

const projectionBand = 0.05

// budgetRules names what a budget of 0 turns off. Positive, not an exclusion
// list: a rule added later must not be resolved by a branch that never heard
// of it.
var budgetRules = map[string]bool{"budget_threshold": true, "budget_projection": true, "resource_share": true}

// daysBilled is how many days of the current month Cost Management has
// actually billed on the as_of date: the figure dated the 5th covers the 1st
// to the 4th. Counting today would understate the pace by a day.
func daysBilled(now, asOf time.Time) int {
	now, asOf = now.UTC(), asOf.UTC()
	if asOf.Year() != now.Year() || asOf.Month() != now.Month() {
		return 0
	}
	return asOf.Day() - 1
}

func daysInMonth(t time.Time) int {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Projection is the rule of three, and it refuses to guess on fewer than
// four billed days.
func Projection(now, asOf time.Time, spent float64) (float64, bool) {
	d := daysBilled(now, asOf)
	if d < 4 {
		return 0, false
	}
	return spent * float64(daysInMonth(now)) / float64(d), true
}

// want records the wanted state of one rule instance; Budget turns wants into
// transitions by comparing them with the journal.
type want struct {
	key   store.GuardrailKey
	on    bool
	value float64
}

func Budget(in BudgetInput) []store.GuardrailEvent {
	var wants []want
	if in.Budget > 0 {
		for _, pct := range in.Settings.Thresholds {
			line := in.Budget * float64(pct) / 100
			wants = append(wants, want{key: store.GuardrailKey{Subject: "budget", Rule: "budget_threshold", Detail: strconv.Itoa(pct)}, on: in.Spent >= line, value: in.Spent})
		}
		pkey := store.GuardrailKey{Subject: "budget", Rule: "budget_projection"}
		if p, ok := Projection(in.Now, in.AsOf, in.Spent); ok {
			firing := in.Last[pkey].Kind == "fired"
			on := p > in.Budget*(1+projectionBand)
			if firing {
				on = p >= in.Budget*(1-projectionBand) // stays on inside the band
			}
			wants = append(wants, want{key: pkey, on: on, value: p})
		} else if in.Last[pkey].Kind == "fired" {
			wants = append(wants, want{key: pkey, on: false, value: 0}) // new month: resolve
		}
		shareLine := in.Budget * float64(in.Settings.ResourceShare) / 100
		seen := map[string]bool{}
		for _, r := range in.Rows {
			seen[r.ResourceID] = true
			wants = append(wants, want{key: store.GuardrailKey{Subject: r.ResourceID, Rule: "resource_share"}, on: r.Amount > shareLine, value: r.Amount / in.Budget * 100})
		}
		// A resource that fired last month and has no row this month resolves.
		for k, e := range in.Last {
			if k.Rule == "resource_share" && e.Kind == "fired" && !seen[k.Subject] {
				wants = append(wants, want{key: k, on: false})
			}
		}
	} else {
		// A budget of 0 disables the budget rules and resolves them. The list
		// is positive on purpose: excluding today's non-budget rules by name
		// would silently resolve any rule added later.
		for k, e := range in.Last {
			if e.Kind == "fired" && budgetRules[k.Rule] {
				wants = append(wants, want{key: k, on: false})
			}
		}
	}
	return transitions(wants, in.Last, in.Now)
}

// transitions is the one place a wanted state becomes a journal line: only
// when it differs from the last line for that instance.
func transitions(wants []want, last map[store.GuardrailKey]store.GuardrailEvent, now time.Time) []store.GuardrailEvent {
	var out []store.GuardrailEvent
	for _, w := range wants {
		firing := last[w.key].Kind == "fired"
		switch {
		case w.on && !firing:
			out = append(out, store.GuardrailEvent{Subject: w.key.Subject, Rule: w.key.Rule, Detail: w.key.Detail, Kind: "fired", Value: w.value, At: now})
		case !w.on && firing:
			out = append(out, store.GuardrailEvent{Subject: w.key.Subject, Rule: w.key.Rule, Detail: w.key.Detail, Kind: "resolved", Value: w.value, At: now})
		}
	}
	return out
}
