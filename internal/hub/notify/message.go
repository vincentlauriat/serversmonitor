// Package notify turns alert transitions into messages and delivers them.
//
// The governing rule of this hub applies here too: silence is never zero. A
// metric that was not collected has no value to report, so a message about it
// says what the rule was, not that the value was 0.
package notify

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Message is one alert transition, in a shape every channel can render.
type Message struct {
	HostID    int64
	HostName  string
	Metric    string
	Kind      string // fired | resolved
	Value     float64
	Threshold float64
	Duration  time.Duration
	At        time.Time
	Link      string // "" when no public URL is configured

	// Guardrail marks a message built by guardrailMessage rather than from a
	// host alert. Title() and Body() need this explicitly: ValueText() == ""
	// is not a safe discriminator between the two, since the implicit status
	// rule (host offline/online) also has an empty ValueText.
	Guardrail bool
}

func (m Message) Fired() bool { return m.Kind == "fired" }

// ValueText renders the value with the unit its metric is measured in.
// The implicit status rule has no value worth showing: "offline 1" means nothing.
//
// Guardrail metrics carry the rule name in the prefix, with the threshold
// detail (if any) appended by guardrailMessage — "budget_threshold 80%" still
// has the "budget_threshold" prefix. resource_share and budget_threshold are
// already computed as a share of the budget, so they render as a percentage;
// budget_projection is a projected currency figure with no currency attached
// here, so it renders as a plain, money-less number. orphan and
// schedule_failed carry nothing worth showing: the rule name is the message.
func (m Message) ValueText() string {
	switch {
	case m.Metric == "resource_share" || strings.HasPrefix(m.Metric, "budget_threshold"):
		return fmt.Sprintf("%.0f %%", m.Value)
	case strings.HasPrefix(m.Metric, "budget_"):
		return fmt.Sprintf("%.0f", m.Value)
	case m.Metric == "orphan" || m.Metric == "schedule_failed":
		return ""
	}
	switch m.Metric {
	case "cpu", "memory", "disk":
		return fmt.Sprintf("%.1f%%", m.Value)
	case "temperature":
		return fmt.Sprintf("%.1f°C", m.Value)
	case "load":
		return fmt.Sprintf("%.2f per core", m.Value)
	case "bandwidth":
		return fmt.Sprintf("%.1f MB/s", m.Value)
	}
	return ""
}

func (m Message) Title() string {
	if m.Metric == "status" {
		if m.Fired() {
			return m.HostName + " is offline"
		}
		return m.HostName + " is back online"
	}
	if m.Guardrail {
		if v := m.ValueText(); v != "" {
			return fmt.Sprintf("%s %s %s (%s)", m.HostName, m.Metric, v, m.Kind)
		}
		// orphan and schedule_failed have no value: a trailing ValueText()
		// would leave a double space between the metric and the "(kind)".
		return fmt.Sprintf("%s %s (%s)", m.HostName, m.Metric, m.Kind)
	}
	return fmt.Sprintf("%s %s %s (%s)", m.HostName, m.Metric, m.ValueText(), m.Kind)
}

func (m Message) Subject() string { return "[ServersMonitor] " + m.Title() }

func (m Message) Body() string {
	var b strings.Builder
	b.WriteString(m.Title())
	b.WriteString("\n\n")
	if m.Guardrail {
		// A guardrail's subject is a budget or an Azure resource, never a
		// host, and it has no threshold/duration rule to restate: the metric
		// already names the rule in the title above.
		fmt.Fprintf(&b, "Subject: %s\n", m.HostName)
		if v := m.ValueText(); v != "" {
			fmt.Fprintf(&b, "Value:   %s\n", v)
		}
		fmt.Fprintf(&b, "Time:    %s\n", m.At.UTC().Format("2006-01-02 15:04:05 UTC"))
		if m.Link != "" {
			fmt.Fprintf(&b, "\n%s\n", m.Link)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Host:   %s\n", m.HostName)
	if v := m.ValueText(); v != "" {
		fmt.Fprintf(&b, "Value:  %s\n", v)
		fmt.Fprintf(&b, "Rule:   above %g for %s\n", m.Threshold, shortDuration(m.Duration))
	} else {
		b.WriteString("Rule:   no sample for three intervals\n")
	}
	fmt.Fprintf(&b, "Time:   %s\n", m.At.UTC().Format("2006-01-02 15:04:05 UTC"))
	if m.Link != "" {
		fmt.Fprintf(&b, "\n%s\n", m.Link)
	}
	return b.String()
}

// shortDuration renders 10m0s as 10m: the trailing zeros read as noise at 3 a.m.
// Written as arithmetic rather than TrimSuffix, because trimming "0m" off "10m"
// leaves "1", and 10m and 30m are two of the four rules seeded by default.
func shortDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// Priority and Tags are ntfy's vocabulary; other receivers ignore them.
func (m Message) Priority() int {
	if m.Fired() {
		return 4
	}
	return 3
}

func (m Message) Tags() []string {
	if m.Fired() {
		return []string{"rotating_light"}
	}
	return []string{"white_check_mark"}
}

type retryableError struct{ err error }

func (r retryableError) Error() string { return r.err.Error() }
func (r retryableError) Unwrap() error { return r.err }

// MarkRetryable says this failure is worth another attempt: a 429, a 5xx, a
// refused connection. A 400 is not, since it will fail identically forever.
func MarkRetryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err}
}

func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var r retryableError
	return errors.As(err, &r)
}
