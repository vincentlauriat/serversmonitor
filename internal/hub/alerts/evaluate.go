package alerts

import (
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// StatusRuleID is the implicit offline rule, which has no row in alert_rules.
const StatusRuleID = 0

type Input struct {
	Now      time.Time
	Interval time.Duration
	Hosts    []store.Host
	Latest   map[int64]store.SampleRow
	Rules    []store.Rule
}

// MetricValue extracts one rule metric from a sample. ok=false means the
// metric was not collected, which must never be read as zero.
func MetricValue(metric string, row store.SampleRow, host store.Host) (float64, bool) {
	switch metric {
	case "cpu":
		if row.CPU == nil {
			return 0, false
		}
		return *row.CPU, true
	case "memory":
		if row.MemUsed == nil || row.MemTotal == nil || *row.MemTotal == 0 {
			return 0, false
		}
		return 100 * float64(*row.MemUsed) / float64(*row.MemTotal), true
	case "disk":
		worst, ok := 0.0, false
		for _, d := range row.Disks {
			if d.Total == 0 {
				continue
			}
			if pct := 100 * float64(d.Used) / float64(d.Total); !ok || pct > worst {
				worst, ok = pct, true
			}
		}
		return worst, ok
	case "temperature":
		hottest, ok := 0.0, false
		for _, t := range row.Temps {
			if !ok || t.Celsius > hottest {
				hottest, ok = t.Celsius, true
			}
		}
		return hottest, ok
	case "load":
		if row.Load5 == nil || host.Cores == 0 {
			return 0, false
		}
		return *row.Load5 / float64(host.Cores), true
	case "bandwidth":
		if row.NetSentBps == nil || row.NetRecvBps == nil {
			return 0, false
		}
		return (*row.NetSentBps + *row.NetRecvBps) / 1e6, true
	}
	return 0, false
}

// Evaluate runs every applicable rule against every host and returns the
// events to persist. Threshold rules only read samples fresher than two
// intervals: a silent host is judged by the status rule alone, never by
// thresholds it can no longer report on.
func Evaluate(m *Machine, in Input) []store.AlertEvent {
	var out []store.AlertEvent
	for _, h := range in.Hosts {
		if h.Status != "never_seen" && !h.Muted {
			breached := h.Status == "offline"
			if tr := m.Observe(Key{StatusRuleID, h.ID}, breached, in.Now, 0); tr != None {
				out = append(out, event(StatusRuleID, h.ID, "status", tr, boolVal(breached), in.Now))
			}
		}
		if h.Muted {
			continue
		}
		row, ok := in.Latest[h.ID]
		if !ok || in.Now.Sub(row.At) > 2*in.Interval {
			continue // silence is not zero
		}
		for _, r := range applicable(in.Rules, h.ID) {
			v, collected := MetricValue(r.Metric, row, h)
			if !collected {
				continue
			}
			if tr := m.Observe(Key{r.ID, h.ID}, v > r.Threshold, in.Now, r.Duration); tr != None {
				out = append(out, event(r.ID, h.ID, r.Metric, tr, v, in.Now))
			}
		}
	}
	return out
}

// applicable returns, per metric, the host-specific rule when one exists, else the global rule.
func applicable(rules []store.Rule, hostID int64) []store.Rule {
	byMetric := map[string]store.Rule{}
	for _, r := range rules {
		if r.HostID != nil && *r.HostID != hostID {
			continue
		}
		cur, seen := byMetric[r.Metric]
		if !seen || (cur.HostID == nil && r.HostID != nil) {
			byMetric[r.Metric] = r
		}
	}
	out := make([]store.Rule, 0, len(byMetric))
	for _, r := range byMetric {
		out = append(out, r)
	}
	return out
}

func event(ruleID, hostID int64, metric string, tr Transition, v float64, now time.Time) store.AlertEvent {
	kind := "fired"
	if tr == Resolved {
		kind = "resolved"
	}
	return store.AlertEvent{RuleID: ruleID, HostID: hostID, Metric: metric, Kind: kind, Value: v, At: now}
}

func boolVal(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
