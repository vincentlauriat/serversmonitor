package hub

import (
	"errors"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/alerts"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// quietNow says whether a channel is in its quiet hours, read in the
// hub-wide zone (the one stop schedules use, set under Settings → Azure). A
// window list that does not parse is treated as no quiet hours: the server
// validates before saving, and a hub that cannot read its own setting must
// over-notify rather than hold everything.
func (h *Hub) quietNow(cfg notify.Config, channel string, now time.Time) bool {
	raw := cfg.Quiet[channel]
	if raw == "" {
		return false
	}
	ws, err := guardrails.ParseWindows(raw)
	if err != nil || len(ws) == 0 {
		return false
	}
	return guardrails.Off(ws, h.GuardrailSettings().Location(), now)
}

// holdFor says whether a transition owed to a channel waits for its quiet
// hours to end. A host going offline, or coming back, never waits: a server
// that falls over at night is worth waking someone for (lot 12).
func (h *Hub) holdFor(cfg notify.Config, channel string, offline bool, now time.Time) bool {
	return !offline && h.quietNow(cfg, channel, now)
}

// remind repeats every fired alert and guardrail whose interval has passed
// since it fired or was last repeated. Called every minute. A channel in its
// quiet hours is skipped, not held: holding would pile a night of reminders
// up for the morning, while skipping lets the next tick after the quiet hours
// send the one reminder that is due.
func (h *Hub) remind(now time.Time) {
	chans := *h.channels.Load()
	if len(chans) == 0 {
		return
	}
	cfg := *h.ncfg.Load()
	var jobs []notify.Job
	jobs = append(jobs, h.remindAlerts(cfg, chans, now)...)
	jobs = append(jobs, h.remindGuardrails(cfg, chans, now)...)
	h.dispatch.Enqueue(jobs...)
}

func (h *Hub) remindAlerts(cfg notify.Config, chans []notify.Channel, now time.Time) []notify.Job {
	last, err := h.st.LastEventPerKey()
	if err != nil {
		h.log.Error("reminders: last events", "err", err)
		return nil
	}
	rules := h.ruleIndex()
	var jobs []notify.Job
	for _, e := range last {
		if e.Kind != notify.KindFired {
			continue
		}
		var every time.Duration
		if e.RuleID == alerts.StatusRuleID {
			every = cfg.OfflineRemind
		} else if r, ok := rules[e.RuleID]; ok {
			every = r.Remind
		} else {
			// A rule deleted while firing never resolves (lot 1): its fired
			// row stays last forever and must not be repeated forever.
			continue
		}
		if every <= 0 {
			continue
		}
		host, err := h.st.Host(e.HostID)
		if err != nil || host.Muted {
			continue
		}
		if !h.due(e.ID, false, e.At, every, now) {
			continue
		}
		also, _ := h.st.FiredChannels(e.RuleID, e.HostID, e.ID+1)
		m := h.message(cfg, e, host, rules)
		m.Kind, m.Since, m.At = notify.KindReminder, e.At, now
		offline := e.RuleID == alerts.StatusRuleID
		for _, ch := range notify.Route(chans, alertRoute(cfg, e, rules), also) {
			if h.holdFor(cfg, ch.Name(), offline, now) {
				continue
			}
			d, err := h.st.AddDelivery(store.DeliverySpec{EventID: e.ID, Channel: ch.Name(), Reminder: true}, now)
			if err != nil {
				h.log.Error("reminders: create delivery", "err", err)
				continue
			}
			jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: m})
		}
	}
	return jobs
}

func (h *Hub) remindGuardrails(cfg notify.Config, chans []notify.Channel, now time.Time) []notify.Job {
	if cfg.GuardrailRemind <= 0 {
		return nil
	}
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("reminders: last guardrail events", "err", err)
		return nil
	}
	names := h.resourceNames()
	var jobs []notify.Job
	for k, e := range last {
		if e.Kind != notify.KindFired || !h.due(e.ID, true, e.At, cfg.GuardrailRemind, now) {
			continue
		}
		also, _ := h.st.GuardrailFiredChannels(k, e.ID+1)
		m := guardrailMessage(cfg, e, names[e.Subject])
		m.Kind, m.Since, m.At = notify.KindReminder, e.At, now
		for _, ch := range notify.Route(chans, cfg.GuardrailRoute, also) {
			if h.holdFor(cfg, ch.Name(), false, now) {
				continue
			}
			d, err := h.st.AddDelivery(store.DeliverySpec{GuardrailEventID: e.ID, Channel: ch.Name(), Reminder: true}, now)
			if err != nil {
				h.log.Error("reminders: create guardrail delivery", "err", err)
				continue
			}
			jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: m})
		}
	}
	return jobs
}

// due reads the last reminder from the deliveries rather than from memory, so
// a restart does not send a burst of reminders a fresh clock thinks are late.
func (h *Hub) due(eventID int64, guardrail bool, firedAt time.Time, every time.Duration, now time.Time) bool {
	since := firedAt
	if at, ok, err := h.st.LastReminder(eventID, guardrail); err != nil {
		return false
	} else if ok {
		since = at
	}
	return now.Sub(since) >= every
}

// releaseHeld sends what waited for a channel's quiet hours, oldest first,
// once they are over. Each row turns pending before it is queued: a crash in
// between leaves a pending row the boot replay picks up. A channel switched
// off meanwhile can never deliver it, and its rows are retired the way the
// replay retires them.
func (h *Hub) releaseHeld(now time.Time) {
	held, err := h.st.HeldDeliveries()
	if err != nil || len(held) == 0 {
		return
	}
	cfg := *h.ncfg.Load()
	byName := h.channelsByName()
	b := h.newRebuilder(cfg)
	var jobs []notify.Job
	for _, d := range held {
		ch, ok := byName[d.Channel]
		if !ok {
			if err := h.st.MarkDeliveryFailed(d.ID, now, 0, "channel disabled"); err != nil {
				h.log.Error("retire held delivery of a disabled channel", "err", err)
			}
			continue
		}
		if h.quietNow(cfg, d.Channel, now) {
			continue
		}
		if err := h.st.ReleaseDelivery(d.ID, now); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				h.log.Error("release held delivery", "err", err)
			}
			continue
		}
		if job, ok := b.job(d, ch); ok {
			jobs = append(jobs, job)
		}
	}
	if len(jobs) > 0 {
		h.log.Info("quiet hours over, sending what was held", "count", len(jobs))
		h.dispatch.Enqueue(jobs...)
	}
}

func (h *Hub) channelsByName() map[string]notify.Channel {
	out := map[string]notify.Channel{}
	for _, c := range *h.channels.Load() {
		out[c.Name()] = c
	}
	return out
}

// rebuilder turns a stored delivery back into the message it carries, for the
// boot replay and for the release of held rows alike. The message keeps the
// event's own time; a reminder keeps the time it was due and says since when.
type rebuilder struct {
	h     *Hub
	cfg   notify.Config
	rules map[int64]store.Rule
	names map[string]string
}

func (h *Hub) newRebuilder(cfg notify.Config) *rebuilder {
	return &rebuilder{h: h, cfg: cfg, rules: h.ruleIndex(), names: h.resourceNames()}
}

func (b *rebuilder) job(d store.Delivery, ch notify.Channel) (notify.Job, bool) {
	var m notify.Message
	if e, host, err := b.h.st.DeliveryEvent(d.ID); err == nil {
		m = b.h.message(b.cfg, e, host, b.rules)
	} else if !errors.Is(err, store.ErrNotFound) {
		return notify.Job{}, false
	} else if ge, err := b.h.st.DeliveryGuardrailEvent(d.ID); err == nil {
		m = guardrailMessage(b.cfg, ge, b.names[ge.Subject])
	} else {
		return notify.Job{}, false // the event is gone, and so is its delivery, by cascade
	}
	if d.Reminder {
		m.Kind, m.Since, m.At = notify.KindReminder, m.At, d.CreatedAt
	}
	return notify.Job{DeliveryID: d.ID, Channel: ch, Message: m}, true
}
