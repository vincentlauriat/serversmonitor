package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// catcher records every webhook POST it receives.
type catcher struct {
	mu  sync.Mutex
	got []notify.WebhookPayload
	hit chan struct{}
	srv *httptest.Server
}

func newCatcher(t *testing.T) *catcher {
	c := &catcher{hit: make(chan struct{}, 32)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var p notify.WebhookPayload
		json.Unmarshal(raw, &p)
		c.mu.Lock()
		c.got = append(c.got, p)
		c.mu.Unlock()
		c.hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *catcher) await(t *testing.T) notify.WebhookPayload {
	t.Helper()
	select {
	case <-c.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("no notification arrived")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[len(c.got)-1]
}

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h, err := New(config.Config{DataDir: t.TempDir()}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestAlertTransitionReachesTheChannel(t *testing.T) {
	c := newCatcher(t)
	h := newTestHub(t)
	if err := notify.SaveConfig(h.st, notify.Config{
		Public:  "https://hub.example",
		Webhook: notify.WebhookConfig{Enabled: true, URL: c.srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.dispatch.Start(ctx)

	host, _, err := h.st.CreateHost("pi-salon", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := h.st.InsertAlertEvent(store.AlertEvent{RuleID: 0, HostID: host.ID,
		Metric: "status", Kind: "fired", Value: 1, At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	h.notify([]store.AlertEvent{saved})

	p := c.await(t)
	if p.Host != "pi-salon" || p.Kind != "fired" || p.Metric != "status" {
		t.Fatalf("payload = %+v", p)
	}
	if p.Link != "https://hub.example/hosts/"+itoa(host.ID) {
		t.Fatalf("link = %q", p.Link)
	}
	waitFor(t, func() bool {
		ds, _ := h.st.ListDeliveries(10)
		return len(ds) == 1 && ds[0].State == "sent"
	}, "the delivery must end up recorded as sent")
}

func TestNoChannelEnabledWritesNoDelivery(t *testing.T) {
	h := newTestHub(t)
	h.ReloadNotify()
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 99, At: time.Now().UTC()})
	h.notify([]store.AlertEvent{saved})
	ds, err := h.st.ListDeliveries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("nothing is configured, so nothing is owed: %+v", ds)
	}
}

func TestPendingDeliveriesAreReplayedOnBoot(t *testing.T) {
	// A hub killed mid-retry must finish the job when it comes back, which is
	// the only reason the pending row is written before the first attempt.
	c := newCatcher(t)
	dir := t.TempDir()
	h1, err := New(config.Config{DataDir: dir}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	notify.SaveConfig(h1.st, notify.Config{Webhook: notify.WebhookConfig{Enabled: true, URL: c.srv.URL}})
	host, _, _ := h1.st.CreateHost("pi", time.Now().UTC())
	saved, _ := h1.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 99, At: time.Now().UTC()})
	// A delivery recorded but never attempted: exactly what a crash leaves behind.
	if _, err := h1.st.CreateDelivery(saved.ID, "webhook", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	h1.Close()

	h2, err := New(config.Config{DataDir: dir}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h2.dispatch.Start(ctx)
	h2.replayPendingDeliveries()
	if p := c.await(t); p.Host != "pi" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestDeliveryDiesWithItsEvent(t *testing.T) {
	// The cascade removes the event with its host, so a pending row can outlive
	// what it describes for as long as it takes the worker to pick it up.
	h := newTestHub(t)
	h.ReloadNotify()
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 99, At: time.Now().UTC()})
	d, _ := h.st.CreateDelivery(saved.ID, "webhook", time.Now().UTC())
	h.st.DeleteHost(host.ID)
	h.replayPendingDeliveries() // must not panic
	if _, err := h.st.Delivery(d.ID); err != store.ErrNotFound {
		t.Fatalf("the delivery went with its event, got %v", err)
	}
}

func TestReplayFailsADeliveryWhoseChannelIsGone(t *testing.T) {
	// Turning a channel off while a delivery is queued leaves a row nothing can
	// ever deliver. Leaving it pending would replay it at every boot forever.
	h := newTestHub(t)
	h.ReloadNotify() // no channel enabled
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 99, At: time.Now().UTC()})
	d, _ := h.st.CreateDelivery(saved.ID, "webhook", time.Now().UTC())
	h.replayPendingDeliveries()
	got, err := h.st.Delivery(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" || got.LastError != "channel disabled" {
		t.Fatalf("delivery = %+v, want failed/channel disabled", got)
	}
	if p, _ := h.st.PendingDeliveries(); len(p) != 0 {
		t.Fatalf("nothing may stay pending, %d left", len(p))
	}
}

func TestMessageCarriesTheRuleThreshold(t *testing.T) {
	h := newTestHub(t)
	h.ReloadNotify()
	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	rule, err := h.st.CreateRule(store.Rule{Metric: "cpu", Threshold: 77, Duration: 10 * time.Minute}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cfg := notify.Config{Public: "https://hub.example"}
	m := h.message(cfg, store.AlertEvent{RuleID: rule.ID, HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 91, At: time.Now().UTC()}, host)
	if m.Threshold != 77 || m.Duration != 10*time.Minute {
		t.Fatalf("message lost the rule: %+v", m)
	}
	if m.Link != "https://hub.example/hosts/"+itoa(host.ID) {
		t.Fatalf("link = %q", m.Link)
	}
	// The implicit status rule has no row, and must not be looked up.
	off := h.message(cfg, store.AlertEvent{RuleID: 0, HostID: host.ID, Metric: "status",
		Kind: "fired", Value: 1, At: time.Now().UTC()}, host)
	if off.Threshold != 0 {
		t.Fatalf("the status rule has no threshold, got %v", off.Threshold)
	}
}

func TestOneTransitionWritesOneDeliveryPerChannel(t *testing.T) {
	// The fan-out itself, asserted before the worker settles anything: a `break`
	// where the loop has `continue`, or a reused delivery id, would leave the
	// rest of the suite green. The dispatcher is deliberately not started.
	c := newCatcher(t)
	h := newTestHub(t)
	if err := notify.SaveConfig(h.st, notify.Config{
		// SMTP points at a port nothing listens on: this test is about the rows
		// that get written, not about whether they are delivered.
		SMTP:    notify.SMTPConfig{Enabled: true, Host: "127.0.0.1", Port: 1, From: "f@example", To: []string{"a@example"}, TLSMode: "none"},
		Webhook: notify.WebhookConfig{Enabled: true, URL: c.srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	h.ReloadNotify()
	if n := len(*h.channels.Load()); n != 2 {
		t.Fatalf("two channels must be enabled, got %d", n)
	}

	host, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu",
		Kind: "fired", Value: 99, At: time.Now().UTC()})
	h.notify([]store.AlertEvent{saved})

	ds, err := h.st.ListDeliveries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("one transition on two channels owes two deliveries, got %d: %+v", len(ds), ds)
	}
	seenID := map[int64]bool{}
	seenChannel := map[string]bool{}
	for _, d := range ds {
		if seenID[d.ID] {
			t.Fatalf("two deliveries share id %d", d.ID)
		}
		if seenChannel[d.Channel] {
			t.Fatalf("channel %s got two rows for one transition", d.Channel)
		}
		if d.EventID != saved.ID {
			t.Fatalf("delivery %d points at event %d, want %d", d.ID, d.EventID, saved.ID)
		}
		seenID[d.ID], seenChannel[d.Channel] = true, true
	}
	if !seenChannel["smtp"] || !seenChannel["webhook"] {
		t.Fatalf("channels = %v, want one row each for smtp and webhook", seenChannel)
	}
}

func TestTwoTransitionsWriteFourDeliveries(t *testing.T) {
	// Two events, two channels. Guards the outer loop the same way.
	c := newCatcher(t)
	h := newTestHub(t)
	notify.SaveConfig(h.st, notify.Config{
		SMTP:    notify.SMTPConfig{Enabled: true, Host: "127.0.0.1", Port: 1, From: "f@example", To: []string{"a@example"}, TLSMode: "none"},
		Webhook: notify.WebhookConfig{Enabled: true, URL: c.srv.URL},
	})
	h.ReloadNotify()
	h1, _, _ := h.st.CreateHost("pi", time.Now().UTC())
	h2, _, _ := h.st.CreateHost("mac", time.Now().UTC())
	e1, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: h1.ID, Metric: "cpu", Kind: "fired", Value: 99, At: time.Now().UTC()})
	e2, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: h2.ID, Metric: "memory", Kind: "resolved", Value: 10, At: time.Now().UTC()})
	h.notify([]store.AlertEvent{e1, e2})
	ds, _ := h.st.ListDeliveries(10)
	if len(ds) != 4 {
		t.Fatalf("two transitions on two channels owe four deliveries, got %d", len(ds))
	}
}
