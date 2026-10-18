package hub

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// deliveriesOf lists an event's delivery rows, with their state and whether
// they are reminders: what remind, notify and releaseHeld leave behind.
func deliveriesOf(t *testing.T, h *Hub, eventID int64, guardrail bool) []store.Delivery {
	t.Helper()
	ds, err := h.st.ListDeliveries(500)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Delivery
	for _, d := range ds {
		id := d.EventID
		if guardrail {
			id = d.GuardrailEventID
		}
		if id != nil && *id == eventID {
			out = append(out, d)
		}
	}
	return out
}

func reminders(ds []store.Delivery) int {
	n := 0
	for _, d := range ds {
		if d.Reminder {
			n++
		}
	}
	return n
}

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) // a Monday, noon UTC

func firedAt(t *testing.T, h *Hub, ruleID, hostID int64, at time.Time) store.AlertEvent {
	t.Helper()
	e, err := h.st.InsertAlertEvent(store.AlertEvent{RuleID: ruleID, HostID: hostID, Metric: "disk", Kind: "fired", Value: 97, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRemindersFollowTheRuleInterval(t *testing.T) {
	h := routingHub(t, nil, nil)
	host, _, _ := h.st.CreateHost("pi", t0)
	r, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90, Remind: time.Hour, Channels: []string{"teams"}}, t0)
	e := firedAt(t, h, r.ID, host.ID, t0)

	h.remind(t0.Add(59 * time.Minute))
	if n := reminders(deliveriesOf(t, h, e.ID, false)); n != 0 {
		t.Fatalf("not due yet, got %d reminders", n)
	}
	h.remind(t0.Add(time.Hour))
	ds := deliveriesOf(t, h, e.ID, false)
	if reminders(ds) != 1 || ds[0].Channel != "teams" {
		t.Fatalf("one reminder on the rule's route, got %+v", ds)
	}
	// Read from the deliveries: the same minute again, or a restarted hub,
	// does not repeat it.
	h.remind(t0.Add(time.Hour + 30*time.Second))
	h.remind(t0.Add(time.Hour + 59*time.Minute))
	if n := reminders(deliveriesOf(t, h, e.ID, false)); n != 1 {
		t.Fatalf("one reminder per interval, got %d", n)
	}
	h.remind(t0.Add(2 * time.Hour))
	if n := reminders(deliveriesOf(t, h, e.ID, false)); n != 2 {
		t.Fatalf("the next interval reminds again, got %d", n)
	}
}

func TestWhatIsNeverReminded(t *testing.T) {
	h := routingHub(t, nil, nil)
	host, _, _ := h.st.CreateHost("pi", t0)
	muted, _, _ := h.st.CreateHost("muted", t0)
	h.st.SetMuted(muted.ID, true)
	never, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90}, t0)
	every, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90, Remind: time.Hour}, t0)
	gone, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90, Remind: time.Hour}, t0)

	noInterval := firedAt(t, h, never.ID, host.ID, t0)
	onMuted := firedAt(t, h, every.ID, muted.ID, t0)
	deleted := firedAt(t, h, gone.ID, host.ID, t0)
	h.st.DeleteRule(gone.ID)
	resolved := firedAt(t, h, every.ID, host.ID, t0)
	h.st.InsertAlertEvent(store.AlertEvent{RuleID: every.ID, HostID: host.ID, Metric: "disk", Kind: "resolved", At: t0.Add(time.Minute)})

	h.remind(t0.Add(24 * time.Hour))
	for name, e := range map[string]store.AlertEvent{"no interval": noInterval, "muted host": onMuted, "deleted rule": deleted, "resolved": resolved} {
		if n := reminders(deliveriesOf(t, h, e.ID, false)); n != 0 {
			t.Errorf("%s: %d reminders", name, n)
		}
	}
}

func TestOfflineAndGuardrailReminders(t *testing.T) {
	h := routingHub(t, []string{"webhook"}, []string{"smtp"})
	c := *h.ncfg.Load()
	c.OfflineRemind, c.GuardrailRemind = 30*time.Minute, 24*time.Hour
	notify.SaveConfig(h.st, c)
	h.ReloadNotify()
	host, _, _ := h.st.CreateHost("pi", t0)
	off := firedAt(t, h, 0, host.ID, t0)
	g, _ := h.st.InsertGuardrailEvent(store.GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "fired", At: t0})

	h.remind(t0.Add(time.Hour))
	if ds := deliveriesOf(t, h, off.ID, false); reminders(ds) != 1 || ds[0].Channel != "webhook" {
		t.Fatalf("offline reminder on its route: %+v", ds)
	}
	if n := reminders(deliveriesOf(t, h, g.ID, true)); n != 0 {
		t.Fatal("the guardrail interval is a day")
	}
	h.remind(t0.Add(25 * time.Hour))
	if ds := deliveriesOf(t, h, g.ID, true); reminders(ds) != 1 || ds[0].Channel != "smtp" {
		t.Fatalf("guardrail reminder on its route: %+v", ds)
	}
}

// quietHub has every channel on, its zone in UTC, and quiet hours on SMTP from
// 11:00 to 13:00 every day, which t0 (noon) falls inside.
func quietHub(t *testing.T) *Hub {
	t.Helper()
	h := routingHub(t, nil, nil)
	h.st.SetSetting("azure_timezone", "UTC")
	c := *h.ncfg.Load()
	c.Quiet = map[string]string{"smtp": `[{"days":[1,2,3,4,5,6,7],"from":"11:00","to":"13:00"}]`}
	notify.SaveConfig(h.st, c)
	h.ReloadNotify()
	h.nowFn = func() time.Time { return t0 }
	return h
}

func byChannel(ds []store.Delivery) map[string]store.Delivery {
	out := map[string]store.Delivery{}
	for _, d := range ds {
		out[d.Channel] = d
	}
	return out
}

func TestQuietHoursHoldThenRelease(t *testing.T) {
	h := quietHub(t)
	host, _, _ := h.st.CreateHost("pi", t0)
	r, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90}, t0)
	e := firedAt(t, h, r.ID, host.ID, t0)
	h.notify([]store.AlertEvent{e})
	got := byChannel(deliveriesOf(t, h, e.ID, false))
	if got["smtp"].State != "held" || got["teams"].State != "pending" || got["webhook"].State != "pending" {
		t.Fatalf("smtp waits, the rest goes: %+v", got)
	}

	h.releaseHeld(t0.Add(30 * time.Minute)) // still quiet
	if byChannel(deliveriesOf(t, h, e.ID, false))["smtp"].State != "held" {
		t.Fatal("released inside the quiet hours")
	}
	h.releaseHeld(t0.Add(61 * time.Minute)) // 13:01
	if s := byChannel(deliveriesOf(t, h, e.ID, false))["smtp"].State; s != "pending" {
		t.Fatalf("after the quiet hours the row is pending and queued, got %s", s)
	}
}

// A host going offline wakes someone up, quiet hours or not.
func TestOfflineIgnoresQuietHours(t *testing.T) {
	h := quietHub(t)
	host, _, _ := h.st.CreateHost("pi", t0)
	e := firedAt(t, h, 0, host.ID, t0)
	h.notify([]store.AlertEvent{e})
	if s := byChannel(deliveriesOf(t, h, e.ID, false))["smtp"].State; s != "pending" {
		t.Fatalf("offline on a quiet channel = %s, want pending", s)
	}
}

func TestGuardrailsRespectQuietHours(t *testing.T) {
	h := quietHub(t)
	g, _ := h.st.InsertGuardrailEvent(store.GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "fired", At: t0})
	h.notifyGuardrails([]store.GuardrailEvent{g})
	if s := byChannel(deliveriesOf(t, h, g.ID, true))["smtp"].State; s != "held" {
		t.Fatalf("guardrail on a quiet channel = %s, want held", s)
	}
}

// A reminder is skipped on a quiet channel, not held: the one that is due
// goes out after the quiet hours, not a night's worth of them.
func TestRemindersSkipQuietChannels(t *testing.T) {
	h := quietHub(t)
	host, _, _ := h.st.CreateHost("pi", t0)
	r, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90, Remind: time.Hour, Channels: []string{"smtp"}}, t0)
	e := firedAt(t, h, r.ID, host.ID, t0.Add(-2*time.Hour))
	h.remind(t0)
	h.remind(t0.Add(30 * time.Minute))
	if ds := deliveriesOf(t, h, e.ID, false); reminders(ds) != 0 {
		t.Fatalf("no reminder while quiet, got %+v", ds)
	}
	h.remind(t0.Add(61 * time.Minute))
	if n := reminders(deliveriesOf(t, h, e.ID, false)); n != 1 {
		t.Fatalf("one reminder once quiet hours end, got %d", n)
	}
}

func TestAHeldRowOfADisabledChannelIsRetired(t *testing.T) {
	h := quietHub(t)
	host, _, _ := h.st.CreateHost("pi", t0)
	r, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90}, t0)
	e := firedAt(t, h, r.ID, host.ID, t0)
	h.notify([]store.AlertEvent{e})
	c := *h.ncfg.Load()
	c.SMTP.Enabled = false
	notify.SaveConfig(h.st, c)
	h.ReloadNotify()
	h.releaseHeld(t0)
	d := byChannel(deliveriesOf(t, h, e.ID, false))["smtp"]
	if d.State != "failed" || d.LastError != "channel disabled" {
		t.Fatalf("held row of a disabled channel = %+v", d)
	}
}

// The boot replay and the release rebuild a reminder as a reminder.
func TestARemindersRowRebuildsAsAReminder(t *testing.T) {
	h := routingHub(t, nil, nil)
	host, _, _ := h.st.CreateHost("pi", t0)
	r, _ := h.st.CreateRule(store.Rule{Metric: "disk", Threshold: 90}, t0)
	e := firedAt(t, h, r.ID, host.ID, t0)
	d, _ := h.st.AddDelivery(store.DeliverySpec{EventID: e.ID, Channel: "smtp", Reminder: true}, t0.Add(4*time.Hour))
	job, ok := h.newRebuilder(*h.ncfg.Load()).job(d, h.channelsByName()["smtp"])
	if !ok || job.Message.Kind != notify.KindReminder || !job.Message.Since.Equal(t0) || !job.Message.At.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("rebuilt = %+v", job.Message)
	}
}
