package hub

import (
	"sort"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// routingHub has the three channels enabled and no dispatcher running: the
// delivery rows are written before anything is sent, so they say where each
// transition was routed without a single network call.
func routingHub(t *testing.T, offline, guardrails []string) *Hub {
	t.Helper()
	h := newTestHub(t)
	if err := notify.SaveConfig(h.st, notify.Config{
		SMTP:           notify.SMTPConfig{Enabled: true, Host: "smtp.invalid", Port: 587, From: "a@b", To: []string{"c@d"}, TLSMode: "starttls"},
		Webhook:        notify.WebhookConfig{Enabled: true, URL: "https://hook.invalid"},
		Teams:          notify.TeamsConfig{Enabled: true, URL: "https://teams.invalid"},
		OfflineRoute:   offline,
		GuardrailRoute: guardrails,
	}); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	return h
}

// channelsOf lists, sorted, the channels that got a delivery for one event.
func channelsOf(t *testing.T, h *Hub, eventID int64, guardrail bool) []string {
	t.Helper()
	ds, err := h.st.ListDeliveries(100)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, d := range ds {
		id := d.EventID
		if guardrail {
			id = d.GuardrailEventID
		}
		if id != nil && *id == eventID {
			out = append(out, d.Channel)
		}
	}
	sort.Strings(out)
	return out
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (h *Hub) alert(t *testing.T, ruleID, hostID int64, kind string) store.AlertEvent {
	t.Helper()
	e, err := h.st.InsertAlertEvent(store.AlertEvent{RuleID: ruleID, HostID: hostID, Metric: "cpu", Kind: kind, Value: 95, At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	h.notify([]store.AlertEvent{e})
	return e
}

func TestRuleRouting(t *testing.T) {
	h := routingHub(t, nil, nil)
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	now := time.Now().UTC()
	every, _ := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 90}, now)
	teams, _ := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 90, Channels: []string{"teams"}}, now)
	nobody, _ := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 90, Channels: []string{}}, now)

	if got := channelsOf(t, h, h.alert(t, every.ID, host.ID, "fired").ID, false); !same(got, []string{"smtp", "teams", "webhook"}) {
		t.Errorf("a rule without a route goes everywhere, got %v", got)
	}
	if got := channelsOf(t, h, h.alert(t, teams.ID, host.ID, "fired").ID, false); !same(got, []string{"teams"}) {
		t.Errorf("a rule routed to Teams goes to Teams only, got %v", got)
	}
	e := h.alert(t, nobody.ID, host.ID, "fired")
	if got := channelsOf(t, h, e.ID, false); len(got) != 0 {
		t.Errorf("a rule routed nowhere writes no delivery, got %v", got)
	}
	if evs, _ := h.st.ListAlertEvents(nil, 10, 0); len(evs) == 0 || evs[0].ID != e.ID {
		t.Error("a rule routed nowhere still records its event")
	}
}

// A channel that heard an alert start hears it end, even when the rule was
// re-routed in between; and the new route hears the end too.
func TestResolvedFollowsTheFired(t *testing.T) {
	h := routingHub(t, nil, nil)
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	r, _ := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 90, Channels: []string{"smtp"}}, time.Now().UTC())
	h.alert(t, r.ID, host.ID, "fired")
	r.Channels = []string{"teams"}
	if err := h.st.UpdateRule(r); err != nil {
		t.Fatal(err)
	}
	if got := channelsOf(t, h, h.alert(t, r.ID, host.ID, "resolved").ID, false); !same(got, []string{"smtp", "teams"}) {
		t.Fatalf("resolved went to %v, want [smtp teams]", got)
	}
}

func TestOfflineRoute(t *testing.T) {
	h := routingHub(t, []string{"webhook"}, nil)
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	if got := channelsOf(t, h, h.alert(t, 0, host.ID, "fired").ID, false); !same(got, []string{"webhook"}) {
		t.Fatalf("offline went to %v, want [webhook]", got)
	}
	// Re-route offline to nowhere: the resolved still reaches the webhook.
	c := *h.ncfg.Load()
	c.OfflineRoute = []string{}
	if err := notify.SaveConfig(h.st, c); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	if got := channelsOf(t, h, h.alert(t, 0, host.ID, "resolved").ID, false); !same(got, []string{"webhook"}) {
		t.Fatalf("offline resolved went to %v, want [webhook]", got)
	}
}

func TestGuardrailRoute(t *testing.T) {
	h := routingHub(t, nil, []string{"smtp"})
	ev := func(kind string) store.GuardrailEvent {
		e, err := h.st.InsertGuardrailEvent(store.GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: kind, At: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		h.notifyGuardrails([]store.GuardrailEvent{e})
		return e
	}
	if got := channelsOf(t, h, ev("fired").ID, true); !same(got, []string{"smtp"}) {
		t.Fatalf("guardrail went to %v, want [smtp]", got)
	}
	c := *h.ncfg.Load()
	c.GuardrailRoute = []string{"teams"}
	if err := notify.SaveConfig(h.st, c); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	if got := channelsOf(t, h, ev("resolved").ID, true); !same(got, []string{"smtp", "teams"}) {
		t.Fatalf("guardrail resolved went to %v, want [smtp teams]", got)
	}
}

// A route naming a channel that is off produces nothing on it and no error:
// enabling that channel later must not require editing the rule again.
func TestRouteToADisabledChannel(t *testing.T) {
	h := newTestHub(t)
	if err := notify.SaveConfig(h.st, notify.Config{Webhook: notify.WebhookConfig{Enabled: true, URL: "https://hook.invalid"}}); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	r, _ := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 90, Channels: []string{"teams", "webhook"}}, time.Now().UTC())
	if got := channelsOf(t, h, h.alert(t, r.ID, host.ID, "fired").ID, false); !same(got, []string{"webhook"}) {
		t.Fatalf("got %v, want [webhook]", got)
	}
}
