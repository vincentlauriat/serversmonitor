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
