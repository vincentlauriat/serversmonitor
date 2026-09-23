package notify

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 9, 18, 3, 14, 0, 0, time.UTC)

func fired() Message {
	return Message{HostID: 1, HostName: "mac-vincent", Metric: "memory", Kind: "fired", Value: 92.4,
		Threshold: 90, Duration: 10 * time.Minute, At: at, Link: "https://hub.example/hosts/1"}
}

func TestValueText(t *testing.T) {
	cases := []struct {
		metric string
		value  float64
		want   string
	}{
		{"memory", 92.4, "92.4%"},
		{"cpu", 7, "7.0%"},
		// 91.05 has no exact float64 representation and lands just below the
		// midpoint, so %.1f rounds it down. Verified against the compiler.
		{"disk", 91.05, "91.0%"},
		{"temperature", 85, "85.0°C"},
		{"load", 2.5, "2.50 per core"},
		{"bandwidth", 3.2, "3.2 MB/s"},
		{"status", 1, ""},
	}
	for _, c := range cases {
		m := Message{Metric: c.metric, Value: c.value}
		if got := m.ValueText(); got != c.want {
			t.Errorf("%s(%v) = %q, want %q", c.metric, c.value, got, c.want)
		}
	}
}

func TestTitleAndSubject(t *testing.T) {
	m := fired()
	if got := m.Title(); got != "mac-vincent memory 92.4% (fired)" {
		t.Fatalf("title = %q", got)
	}
	if got := m.Subject(); got != "[ServersMonitor] mac-vincent memory 92.4% (fired)" {
		t.Fatalf("subject = %q", got)
	}
	off := Message{HostName: "pi-salon", Metric: "status", Kind: "fired"}
	if got := off.Title(); got != "pi-salon is offline" {
		t.Fatalf("offline title = %q", got)
	}
	back := Message{HostName: "pi-salon", Metric: "status", Kind: "resolved"}
	if got := back.Title(); got != "pi-salon is back online" {
		t.Fatalf("recovery title = %q", got)
	}
}

func TestBodyCarriesTheThresholdAndTheLink(t *testing.T) {
	b := fired().Body()
	for _, want := range []string{"mac-vincent", "92.4%", "90", "10m", "https://hub.example/hosts/1"} {
		if !strings.Contains(b, want) {
			t.Fatalf("body is missing %q:\n%s", want, b)
		}
	}
	noLink := fired()
	noLink.Link = ""
	if strings.Contains(noLink.Body(), "http") {
		t.Fatalf("no link configured means no link in the body:\n%s", noLink.Body())
	}
}

func TestShortDurationDoesNotEatTrailingZeros(t *testing.T) {
	// TrimSuffix("10m", "0m") leaves "1". Two of the four seeded rules are 10m
	// and 30m, so this is the default case, not an edge case.
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "10m",
		30 * time.Minute: "30m",
		5 * time.Minute:  "5m",
		time.Hour:        "1h",
		90 * time.Second: "1m30s",
		0:                "0s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestResolvedIsDistinguishable(t *testing.T) {
	m := fired()
	m.Kind = "resolved"
	m.Value = 41
	if m.Fired() {
		t.Fatal("Fired must be false")
	}
	if !strings.Contains(m.Title(), "resolved") {
		t.Fatalf("title = %q", m.Title())
	}
	if m.Priority() >= fired().Priority() {
		t.Fatal("a recovery must not shout louder than the alert")
	}
	if len(m.Tags()) == 0 || m.Tags()[0] == fired().Tags()[0] {
		t.Fatalf("tags = %v", m.Tags())
	}
}

func TestGuardrailRenderingIsFactualNotAlertCopy(t *testing.T) {
	// An orphan guardrail has no ValueText (like the implicit status rule),
	// but it is not a host and must not borrow the host-alert copy.
	m := Message{HostName: "d1", Metric: "orphan", Kind: "fired", At: at, Guardrail: true}
	title := m.Title()
	if strings.Contains(title, "  ") {
		t.Fatalf("title has a double space: %q", title)
	}
	if title != "d1 orphan (fired)" {
		t.Fatalf("title = %q", title)
	}
	body := m.Body()
	if strings.Contains(body, "Host:") {
		t.Fatalf("a guardrail subject is not a host:\n%s", body)
	}
	if strings.Contains(body, "no sample for three intervals") {
		t.Fatalf("d1 is a disk, not a host with a missed sample:\n%s", body)
	}
	if !strings.Contains(body, "Subject: d1") {
		t.Fatalf("body is missing the Subject line:\n%s", body)
	}

	// The status rule (host offline/online) also has an empty ValueText, and
	// must keep the original alert copy: Guardrail is false by zero value.
	off := Message{HostName: "pi-salon", Metric: "status", Kind: "fired", At: at}
	if got := off.Title(); got != "pi-salon is offline" {
		t.Fatalf("status title regressed: %q", got)
	}
	if !strings.Contains(off.Body(), "Host:   pi-salon") {
		t.Fatalf("status body regressed:\n%s", off.Body())
	}
}

func TestGuardrailWithAValueStillShowsIt(t *testing.T) {
	// budget_threshold, resource_share and budget_projection carry a value
	// and must keep showing it, just without the alert-only Rule line.
	m := Message{HostName: "Budget", Metric: "budget_threshold 80%", Kind: "fired", Value: 95, At: at, Guardrail: true}
	title := m.Title()
	if !strings.Contains(title, "95 %") {
		t.Fatalf("title dropped the value: %q", title)
	}
	body := m.Body()
	if !strings.Contains(body, "Value:   95 %") {
		t.Fatalf("body dropped the value:\n%s", body)
	}
	if strings.Contains(body, "Rule:") {
		t.Fatalf("a guardrail body must not restate a threshold/duration rule:\n%s", body)
	}
}

func TestRetryableMarking(t *testing.T) {
	base := errors.New("boom")
	if Retryable(base) {
		t.Fatal("a plain error is not retryable")
	}
	wrapped := MarkRetryable(base)
	if !Retryable(wrapped) {
		t.Fatal("a marked error is retryable")
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("marking must not hide the original error")
	}
	if !strings.Contains(wrapped.Error(), "boom") {
		t.Fatalf("message lost: %q", wrapped.Error())
	}
	if Retryable(nil) {
		t.Fatal("nil is not retryable")
	}
}
