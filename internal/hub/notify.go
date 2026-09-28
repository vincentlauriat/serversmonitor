package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/alerts"
	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// ReloadNotify rebuilds the channel list from the settings. Called at boot and
// whenever the notifications settings are saved. Exported because the server
// reaches it through its Notifier interface.
func (h *Hub) ReloadNotify() {
	c := notify.LoadConfig(h.st)
	chans := notify.Enabled(c)
	h.ncfg.Store(&c)
	h.channels.Store(&chans)
}

// notify records one pending delivery per channel the event is routed to,
// then hands the work to the dispatcher. The row is written first: a crash
// between here and the send leaves evidence, not silence.
func (h *Hub) notify(events []store.AlertEvent) {
	chans := *h.channels.Load()
	if len(chans) == 0 || len(events) == 0 {
		return
	}
	cfg := *h.ncfg.Load()
	rules := h.ruleIndex()
	now := time.Now().UTC()
	var jobs []notify.Job
	for _, e := range events {
		host, err := h.st.Host(e.HostID)
		if err != nil {
			h.log.Error("notify: host", "err", err, "host_id", e.HostID)
			continue
		}
		var also []string
		if e.Kind == "resolved" {
			if also, err = h.st.FiredChannels(e.RuleID, e.HostID, e.ID); err != nil {
				h.log.Error("notify: channels of the fired", "err", err)
			}
		}
		m := h.message(cfg, e, host, rules)
		for _, ch := range notify.Route(chans, alertRoute(cfg, e, rules), also) {
			d, err := h.st.CreateDelivery(e.ID, ch.Name(), now)
			if err != nil {
				h.log.Error("notify: create delivery", "err", err)
				continue
			}
			jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: m})
		}
	}
	h.dispatch.Enqueue(jobs...)
}

// alertRoute is where an alert transition goes: the offline setting for the
// implicit status rule, the rule's own route otherwise. A rule deleted since
// the event goes everywhere, the same way its message keeps a zero threshold
// rather than blocking: over-notifying is the failure a person can see.
func alertRoute(cfg notify.Config, e store.AlertEvent, rules map[int64]store.Rule) []string {
	if e.RuleID == alerts.StatusRuleID {
		return cfg.OfflineRoute
	}
	if r, ok := rules[e.RuleID]; ok {
		return r.Channels
	}
	return nil
}

// ruleIndex reads the rules once for a batch of events. An error leaves it
// empty, which routes everywhere and messages with a zero threshold.
func (h *Hub) ruleIndex() map[int64]store.Rule {
	out := map[int64]store.Rule{}
	rules, err := h.st.ListRules()
	if err != nil {
		h.log.Error("notify: rules", "err", err)
		return out
	}
	for _, r := range rules {
		out[r.ID] = r
	}
	return out
}

// message turns a stored event into what a channel renders, looking up the
// threshold of the rule that fired. A rule deleted since keeps a zero
// threshold rather than blocking the notification.
func (h *Hub) message(cfg notify.Config, e store.AlertEvent, host store.Host, rules map[int64]store.Rule) notify.Message {
	m := notify.Message{HostID: host.ID, HostName: host.Name, Metric: e.Metric,
		Kind: e.Kind, Value: e.Value, At: e.At, Link: cfg.Link(host.ID)}
	if r, ok := rules[e.RuleID]; ok && e.RuleID != alerts.StatusRuleID {
		m.Threshold, m.Duration = r.Threshold, r.Duration
	}
	return m
}

// notifyGuardrails is journalGuardrails' delivery half: one delivery row per
// routed channel per transition, the same shape the alert path uses, and the
// same rule for a resolved: it also goes wherever its fired went.
func (h *Hub) notifyGuardrails(events []store.GuardrailEvent) {
	chans := *h.channels.Load()
	if len(chans) == 0 || len(events) == 0 {
		return
	}
	cfg := *h.ncfg.Load()
	now := time.Now().UTC()
	names := h.resourceNames()
	var jobs []notify.Job
	for _, e := range events {
		var also []string
		if e.Kind == "resolved" {
			var err error
			k := store.GuardrailKey{Subject: e.Subject, Rule: e.Rule, Detail: e.Detail}
			if also, err = h.st.GuardrailFiredChannels(k, e.ID); err != nil {
				h.log.Error("notify: channels of the guardrail fired", "err", err)
			}
		}
		m := guardrailMessage(cfg, e, names[e.Subject])
		for _, ch := range notify.Route(chans, cfg.GuardrailRoute, also) {
			d, err := h.st.CreateGuardrailDelivery(e.ID, ch.Name(), now)
			if err != nil {
				h.log.Error("notify: create guardrail delivery", "err", err)
				continue
			}
			jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: m})
		}
	}
	h.dispatch.Enqueue(jobs...)
}

// guardrailMessage reuses the alert shape: the subject stands where the host
// name stands, the rule where the metric stands. Channels need no change.
func guardrailMessage(cfg notify.Config, e store.GuardrailEvent, name string) notify.Message {
	subject := name
	if e.Subject == "budget" {
		subject = "Budget"
	} else if subject == "" {
		subject = azure.LastSegment(e.Subject)
	}
	metric := e.Rule
	if e.Detail != "" {
		metric = e.Rule + " " + e.Detail + "%"
	}
	return notify.Message{HostName: subject, Metric: metric, Kind: e.Kind, Value: e.Value, At: e.At, Link: cfg.AzureLink(), Guardrail: true}
}

// replayPendingDeliveries picks up what a previous run left unfinished.
//
// Delivery is at least once, deliberately. The pending row is written before
// the first attempt, so a crash between a channel's 200 and MarkDeliverySent
// replays the message on the next boot and Vincent gets it twice. A duplicate
// alert is a nuisance; a lost one is the failure this whole lot exists to
// prevent. The README says so, so a second mail is not read as a bug.
func (h *Hub) replayPendingDeliveries() {
	pending, err := h.st.PendingDeliveries()
	if err != nil {
		h.log.Error("replay deliveries", "err", err)
		return
	}
	byName := map[string]notify.Channel{}
	for _, c := range *h.channels.Load() {
		byName[c.Name()] = c
	}
	cfg := *h.ncfg.Load()
	names := h.resourceNames()
	rules := h.ruleIndex()
	var jobs []notify.Job
	for _, d := range pending {
		ch, ok := byName[d.Channel]
		if !ok {
			// The channel was turned off while this was queued. Nothing can
			// deliver it, and leaving it pending would replay it at every boot.
			if err := h.st.MarkDeliveryFailed(d.ID, time.Now().UTC(), 0, "channel disabled"); err != nil {
				h.log.Error("retire delivery of a disabled channel", "err", err)
			}
			continue
		}
		e, host, err := h.st.DeliveryEvent(d.ID)
		if err == nil {
			jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: h.message(cfg, e, host, rules)})
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			continue
		}
		ge, err := h.st.DeliveryGuardrailEvent(d.ID)
		if err != nil {
			continue // the event is gone, and so is its delivery, by cascade
		}
		jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: guardrailMessage(cfg, ge, names[ge.Subject])})
	}
	if len(jobs) > 0 {
		h.log.Info("replaying deliveries left by a previous run", "count", len(jobs))
		h.dispatch.Enqueue(jobs...)
	}
}

// TestNotify sends one synthetic message through a single channel and returns
// the channel's own error. It does not go through the dispatcher: a test must
// answer now, with the real reason, not four retries later in a log.
func (h *Hub) TestNotify(ctx context.Context, name string) error {
	for _, ch := range *h.channels.Load() {
		if ch.Name() != name {
			continue
		}
		cfg := *h.ncfg.Load()
		return ch.Send(ctx, notify.Message{
			HostName: "serversmonitor", Metric: "cpu", Kind: "fired", Value: 99.9,
			Threshold: 90, Duration: 10 * time.Minute, At: time.Now().UTC(), Link: cfg.Root(),
		})
	}
	return fmt.Errorf("channel %s is not enabled", name)
}
