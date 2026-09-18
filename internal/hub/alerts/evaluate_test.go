package alerts

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

var now = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }
func i(v int64) *int64     { return &v }

func host(id int64, status string) store.Host {
	return store.Host{ID: id, Name: "h", Status: status, Cores: 4}
}

func fresh(cpu float64) store.SampleRow {
	return store.SampleRow{At: now.Add(-5 * time.Second), CPU: f(cpu), MemUsed: i(90), MemTotal: i(100),
		Disks: []proto.Disk{{Mount: "/", Used: 50, Total: 100}, {Mount: "/data", Used: 95, Total: 100}},
		Temps: []proto.Temp{{Sensor: "cpu", Celsius: 70}, {Sensor: "nvme", Celsius: 85}},
		Load5: f(8), NetSentBps: f(1e6), NetRecvBps: f(2e6)}
}

func TestMetricValue(t *testing.T) {
	row := fresh(42)
	h := host(1, "online")
	cases := map[string]float64{"cpu": 42, "memory": 90, "disk": 95, "temperature": 85, "load": 2, "bandwidth": 3}
	for metric, want := range cases {
		got, ok := MetricValue(metric, row, h)
		if !ok || got != want {
			t.Fatalf("%s = %v %v, want %v", metric, got, ok, want)
		}
	}
	if _, ok := MetricValue("cpu", store.SampleRow{}, h); ok {
		t.Fatal("missing cpu must be not collected")
	}
	if _, ok := MetricValue("disk", store.SampleRow{Disks: []proto.Disk{{Mount: "/x", Total: 0}}}, h); ok {
		t.Fatal("a zero-sized disk must not count as collected")
	}
	if _, ok := MetricValue("teleport", row, h); ok {
		t.Fatal("unknown metric must be not collected")
	}
}

func TestEvaluateFiresAndResolvesThreshold(t *testing.T) {
	m := NewMachine()
	rules := []store.Rule{{ID: 1, Metric: "cpu", Threshold: 90, Duration: 0}}
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "online")}, Latest: map[int64]store.SampleRow{1: fresh(95)}, Rules: rules}
	evs := Evaluate(m, in)
	if len(evs) != 1 || evs[0].Kind != "fired" || evs[0].Metric != "cpu" || evs[0].Value != 95 || evs[0].RuleID != 1 {
		t.Fatalf("evs = %+v", evs)
	}
	in.Latest = map[int64]store.SampleRow{1: fresh(10)}
	evs = Evaluate(m, in)
	if len(evs) != 1 || evs[0].Kind != "resolved" || evs[0].Value != 10 {
		t.Fatalf("evs = %+v", evs)
	}
}

func TestEvaluateSilenceIsNotZero(t *testing.T) {
	m := NewMachine()
	rules := []store.Rule{{ID: 1, Metric: "cpu", Threshold: 90, Duration: 0}}
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "online")}, Latest: map[int64]store.SampleRow{1: fresh(95)}, Rules: rules}
	Evaluate(m, in) // fired
	stale := fresh(95)
	stale.At = now.Add(-time.Minute) // older than two intervals
	in.Latest = map[int64]store.SampleRow{1: stale}
	if evs := Evaluate(m, in); len(evs) != 0 {
		t.Fatalf("stale sample must not resolve or fire thresholds: %+v", evs)
	}
	if m.State(Key{1, 1}) != Firing {
		t.Fatal("state must be untouched by a stale sample")
	}
}

func TestEvaluateNoSampleAtAllIsInertOnThresholds(t *testing.T) {
	m := NewMachine()
	rules := []store.Rule{{ID: 1, Metric: "cpu", Threshold: 0, Duration: 0}}
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "online")}, Latest: map[int64]store.SampleRow{}, Rules: rules}
	if evs := Evaluate(m, in); len(evs) != 0 {
		t.Fatalf("a host with no sample must not fire a threshold: %+v", evs)
	}
}

func TestEvaluateStatusRule(t *testing.T) {
	m := NewMachine()
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "offline")}, Latest: map[int64]store.SampleRow{}}
	evs := Evaluate(m, in)
	if len(evs) != 1 || evs[0].RuleID != StatusRuleID || evs[0].Metric != "status" || evs[0].Kind != "fired" {
		t.Fatalf("evs = %+v", evs)
	}
	in.Hosts[0].Status = "online"
	if evs := Evaluate(m, in); len(evs) != 1 || evs[0].Kind != "resolved" {
		t.Fatalf("evs = %+v", evs)
	}
}

func TestEvaluateNeverSeenIsInert(t *testing.T) {
	m := NewMachine()
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "never_seen")}}
	if evs := Evaluate(m, in); len(evs) != 0 {
		t.Fatalf("a host never seen cannot have disappeared: %+v", evs)
	}
}

func TestEvaluateMutedHostIsFullySilent(t *testing.T) {
	m := NewMachine()
	muted := host(2, "offline")
	muted.Muted = true
	rules := []store.Rule{{ID: 1, Metric: "cpu", Threshold: 0, Duration: 0}}
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{muted}, Latest: map[int64]store.SampleRow{2: fresh(95)}, Rules: rules}
	if evs := Evaluate(m, in); len(evs) != 0 {
		t.Fatalf("muting a host silences its thresholds too, not only its offline rule: %+v", evs)
	}
}

func TestEvaluatePerHostOverridesGlobal(t *testing.T) {
	m := NewMachine()
	h1 := int64(1)
	rules := []store.Rule{{ID: 1, Metric: "cpu", Threshold: 50}, {ID: 2, HostID: &h1, Metric: "cpu", Threshold: 99}}
	in := Input{Now: now, Interval: 10 * time.Second, Hosts: []store.Host{host(1, "online"), host(2, "online")},
		Latest: map[int64]store.SampleRow{1: fresh(80), 2: fresh(80)}, Rules: rules}
	evs := Evaluate(m, in)
	if len(evs) != 1 || evs[0].HostID != 2 || evs[0].RuleID != 1 {
		t.Fatalf("host 1 must use its own rule (99), host 2 the global (50): %+v", evs)
	}
	// Same rules in the reverse order must give the same answer.
	m2 := NewMachine()
	in.Rules = []store.Rule{{ID: 2, HostID: &h1, Metric: "cpu", Threshold: 99}, {ID: 1, Metric: "cpu", Threshold: 50}}
	evs = Evaluate(m2, in)
	if len(evs) != 1 || evs[0].HostID != 2 {
		t.Fatalf("rule order must not matter: %+v", evs)
	}
}
