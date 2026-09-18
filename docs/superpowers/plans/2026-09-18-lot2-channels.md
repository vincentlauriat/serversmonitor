# ServersMonitor Lot 2 (Alert channels) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Every `fired` and `resolved` transition reaches Vincent through SMTP, a generic webhook (ntfy-shaped) and Microsoft Teams, with a durable record of whether it actually arrived.

**Architecture:** `internal/hub/notify` owns everything. `Message` is the channel-agnostic payload; `Channel` is a one-method interface; `Dispatcher` holds the queue, the worker and the retry policy. `Hub.evaluate` writes the event, writes one `pending` delivery row per enabled channel, then hands the ids to the dispatcher. Channel settings live in `settings`, edited from a Notifications tab.

**Tech Stack:** Go standard library only for SMTP (`net/smtp`) and HTTP. No new dependency.

**Spec:** `docs/superpowers/specs/2026-09-18-lot2-alert-channels-design.md`

## Global Constraints

- Branch `feat/lot2-channels`; never commit to `main`. Conventional commits, English, no `Co-Authored-By` trailer.
- **The delivery row is written before the first attempt.** The worker is the only thing that moves it to `sent` or `failed`.
- Only `fired` and `resolved` produce deliveries. Nothing else may call the dispatcher.
- Retries: 4 attempts, base 1 s, factor 5, so ~1 s, 5 s, 25 s. Retry `429` and `5xx`; do not retry other `4xx`.
- Queue full drops the oldest `fired` first, a `resolved` only when nothing else is left; the row becomes `failed` with reason `queue full`.
- The SMTP password never leaves the API. Reads expose `smtp_password_set` (bool) only.
- `go test ./...` passes with no network: every channel is tested against a local listener or an injected transport.
- `go vet ./...` clean; `svelte-check` with zero errors.

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/hub/store/migrations/0002_deliveries.sql` | `deliveries` table |
| `internal/hub/store/deliveries.go` | insert, claim, mark sent/failed, list, purge |
| `internal/hub/notify/message.go` | `Message`, its rendering helpers |
| `internal/hub/notify/channel.go` | `Channel` interface, `Config`, `ConfigFromStore` |
| `internal/hub/notify/smtp.go` | SMTP channel |
| `internal/hub/notify/webhook.go` | generic webhook / ntfy channel |
| `internal/hub/notify/teams.go` | Teams Adaptive Card channel |
| `internal/hub/notify/dispatcher.go` | queue, worker, retry, drop policy |
| `internal/hub/hub.go` | wire the dispatcher, enqueue after persisting events |
| `internal/hub/server/api_notify.go` | `GET/PUT /api/v1/notifications`, `POST .../test`, `GET .../deliveries` |
| `web/src/routes/settings/+page.svelte` | Notifications tab |

---

### Task 1: The deliveries table

**Files:**
- Create: `internal/hub/store/migrations/0002_deliveries.sql`, `internal/hub/store/deliveries.go`
- Test: `internal/hub/store/deliveries_test.go`

**Interfaces:**
- Produces:
```go
type Delivery struct { ID int64; EventID int64; Channel string; State string /* pending|sent|failed */; Attempts int; LastError string; CreatedAt time.Time; UpdatedAt time.Time }
func (s *Store) CreateDelivery(eventID int64, channel string, now time.Time) (Delivery, error)
func (s *Store) MarkDeliverySent(id int64, now time.Time, attempts int) error
func (s *Store) MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error
func (s *Store) PendingDeliveries() ([]Delivery, error)          // oldest first, for restart recovery
func (s *Store) ListDeliveries(limit int) ([]Delivery, error)    // newest first
func (s *Store) Delivery(id int64) (Delivery, error)
func (s *Store) DeliveryEvent(id int64) (AlertEvent, Host, error) // the event and its host, for rendering
func (s *Store) PurgeDeliveries(now time.Time, keep time.Duration) error
func (s *Store) ChannelHealth() (map[string]Delivery, error)     // last delivery per channel
```

- [ ] **Step 1: Write the migration**

`internal/hub/store/migrations/0002_deliveries.sql`:
```sql
CREATE TABLE deliveries (
  id         INTEGER PRIMARY KEY,
  event_id   INTEGER NOT NULL REFERENCES alert_events(id) ON DELETE CASCADE,
  channel    TEXT NOT NULL,            -- smtp | webhook | teams
  state      TEXT NOT NULL,            -- pending | sent | failed
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX deliveries_state ON deliveries(state, id);
CREATE INDEX deliveries_channel ON deliveries(channel, id);
```

The foreign key matters: deleting a host cascades to its events, and its events must cascade to their deliveries rather than leave orphans the health view would count.

- [ ] **Step 2: Write the failing tests**

`internal/hub/store/deliveries_test.go`:
```go
package store

import (
	"testing"
	"time"
)

func seedEvent(t *testing.T, s *Store, hostID int64, kind string, at time.Time) AlertEvent {
	t.Helper()
	e, err := s.InsertAlertEvent(AlertEvent{RuleID: 1, HostID: hostID, Metric: "cpu", Kind: kind, Value: 95, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeliveryLifecycle(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	d, err := s.CreateDelivery(e.ID, "smtp", t0)
	if err != nil || d.ID == 0 || d.State != "pending" || d.Attempts != 0 {
		t.Fatalf("create = %+v %v", d, err)
	}
	pending, _ := s.PendingDeliveries()
	if len(pending) != 1 || pending[0].ID != d.ID {
		t.Fatalf("pending = %+v", pending)
	}
	if err := s.MarkDeliverySent(d.ID, t0.Add(time.Second), 2); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Delivery(d.ID)
	if got.State != "sent" || got.Attempts != 2 || got.LastError != "" {
		t.Fatalf("sent = %+v", got)
	}
	if p, _ := s.PendingDeliveries(); len(p) != 0 {
		t.Fatal("a sent delivery is no longer pending")
	}
}

func TestDeliveryFailureKeepsTheReason(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	d, _ := s.CreateDelivery(e.ID, "webhook", t0)
	if err := s.MarkDeliveryFailed(d.ID, t0.Add(time.Minute), 4, "503 Service Unavailable"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Delivery(d.ID)
	if got.State != "failed" || got.Attempts != 4 || got.LastError != "503 Service Unavailable" {
		t.Fatalf("failed = %+v", got)
	}
}

func TestDeliveryEventCarriesItsHost(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "resolved", t0)
	d, _ := s.CreateDelivery(e.ID, "teams", t0)
	ev, host, err := s.DeliveryEvent(d.ID)
	if err != nil || ev.ID != e.ID || ev.Kind != "resolved" || host.Name != "pi" {
		t.Fatalf("event = %+v host = %+v err = %v", ev, host, err)
	}
	if _, _, err := s.DeliveryEvent(9999); err != ErrNotFound {
		t.Fatalf("missing delivery = %v", err)
	}
}

func TestDeliveriesCascadeWithTheHost(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	s.CreateDelivery(e.ID, "smtp", t0)
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("fixture did not land: %d", n)
	}
	if err := s.DeleteHost(h.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 0 {
		t.Fatalf("deleting a host must cascade to deliveries, %d left", n)
	}
}

func TestChannelHealthKeepsTheLatestPerChannel(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e1 := seedEvent(t, s, h.ID, "fired", t0)
	e2 := seedEvent(t, s, h.ID, "resolved", t0.Add(time.Minute))
	d1, _ := s.CreateDelivery(e1.ID, "smtp", t0)
	s.MarkDeliveryFailed(d1.ID, t0, 4, "boom")
	d2, _ := s.CreateDelivery(e2.ID, "smtp", t0.Add(time.Minute))
	s.MarkDeliverySent(d2.ID, t0.Add(time.Minute), 1)
	d3, _ := s.CreateDelivery(e1.ID, "webhook", t0)
	s.MarkDeliveryFailed(d3.ID, t0, 4, "dns")
	health, err := s.ChannelHealth()
	if err != nil {
		t.Fatal(err)
	}
	if health["smtp"].State != "sent" {
		t.Fatalf("smtp health must be the latest, got %+v", health["smtp"])
	}
	if health["webhook"].LastError != "dns" {
		t.Fatalf("webhook health = %+v", health["webhook"])
	}
	if _, ok := health["teams"]; ok {
		t.Fatal("a channel that never delivered has no health row")
	}
}

func TestPurgeDeliveries(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	old, _ := s.CreateDelivery(e.ID, "smtp", t0)
	s.MarkDeliverySent(old.ID, t0, 1)
	recent, _ := s.CreateDelivery(e.ID, "webhook", t0.Add(48*time.Hour))
	s.MarkDeliverySent(recent.ID, t0.Add(48*time.Hour), 1)
	if err := s.PurgeDeliveries(t0.Add(49*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("purge left %d rows, want 1", n)
	}
}

func TestPurgeKeepsPendingWhateverItsAge(t *testing.T) {
	// A delivery still pending is work not yet done; age must not delete it,
	// or a hub that was offline for a day comes back with nothing to send.
	s := openTest(t)
	h, _, _ := s.CreateHost("pi", t0)
	e := seedEvent(t, s, h.ID, "fired", t0)
	s.CreateDelivery(e.ID, "smtp", t0)
	if err := s.PurgeDeliveries(t0.Add(365*24*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "deliveries"); n != 1 {
		t.Fatalf("a pending delivery must survive the purge, %d left", n)
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

Run: `go test ./internal/hub/store/ -run Deliver`
Expected: FAIL, `undefined: CreateDelivery`.

- [ ] **Step 4: Write deliveries.go**

```go
package store

import (
	"database/sql"
	"errors"
	"time"
)

type Delivery struct {
	ID        int64
	EventID   int64
	Channel   string
	State     string
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const deliveryCols = `id, event_id, channel, state, attempts, last_error, created_at, updated_at`

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	var created, updated string
	err := row.Scan(&d.ID, &d.EventID, &d.Channel, &d.State, &d.Attempts, &d.LastError, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if d.CreatedAt, err = parseTime(created); err != nil {
		return d, err
	}
	d.UpdatedAt, err = parseTime(updated)
	return d, err
}

// CreateDelivery records the intent to deliver, before anything is attempted.
// A crash between here and the first send leaves a pending row, which is what
// makes the queue survive a restart.
func (s *Store) CreateDelivery(eventID int64, channel string, now time.Time) (Delivery, error) {
	at := fmtTime(now)
	res, err := s.db.Exec(`INSERT INTO deliveries(event_id, channel, state, created_at, updated_at) VALUES (?,?, 'pending', ?, ?)`,
		eventID, channel, at, at)
	if err != nil {
		return Delivery{}, err
	}
	id, _ := res.LastInsertId()
	return s.Delivery(id)
}

func (s *Store) Delivery(id int64) (Delivery, error) {
	return scanDelivery(s.db.QueryRow(`SELECT `+deliveryCols+` FROM deliveries WHERE id = ?`, id))
}

func (s *Store) MarkDeliverySent(id int64, now time.Time, attempts int) error {
	return s.execOne(`UPDATE deliveries SET state='sent', attempts=?, last_error='', updated_at=? WHERE id=?`, attempts, fmtTime(now), id)
}

func (s *Store) MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error {
	return s.execOne(`UPDATE deliveries SET state='failed', attempts=?, last_error=?, updated_at=? WHERE id=?`, attempts, reason, fmtTime(now), id)
}

func (s *Store) queryDeliveries(q string, args ...any) ([]Delivery, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) PendingDeliveries() ([]Delivery, error) {
	return s.queryDeliveries(`SELECT ` + deliveryCols + ` FROM deliveries WHERE state='pending' ORDER BY id`)
}

func (s *Store) ListDeliveries(limit int) ([]Delivery, error) {
	return s.queryDeliveries(`SELECT `+deliveryCols+` FROM deliveries ORDER BY id DESC LIMIT ?`, limit)
}

// DeliveryEvent returns what a delivery is about: its event and that event's host.
func (s *Store) DeliveryEvent(id int64) (AlertEvent, Host, error) {
	var eventID int64
	err := s.db.QueryRow(`SELECT event_id FROM deliveries WHERE id = ?`, id).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertEvent{}, Host{}, ErrNotFound
	}
	if err != nil {
		return AlertEvent{}, Host{}, err
	}
	rows, err := s.db.Query(`SELECT id, rule_id, host_id, metric, kind, value, at FROM alert_events WHERE id = ?`, eventID)
	if err != nil {
		return AlertEvent{}, Host{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return AlertEvent{}, Host{}, ErrNotFound
	}
	e, err := scanEvent(rows)
	if err != nil {
		return AlertEvent{}, Host{}, err
	}
	h, err := s.Host(e.HostID)
	return e, h, err
}

// ChannelHealth returns the most recent delivery of each channel, which is what
// the settings page shows: "configured" and "working" are different claims.
func (s *Store) ChannelHealth() (map[string]Delivery, error) {
	ds, err := s.queryDeliveries(`SELECT ` + deliveryCols + ` FROM deliveries d
		WHERE d.id = (SELECT max(id) FROM deliveries WHERE channel = d.channel)`)
	if err != nil {
		return nil, err
	}
	out := map[string]Delivery{}
	for _, d := range ds {
		out[d.Channel] = d
	}
	return out, nil
}

// PurgeDeliveries drops settled rows older than keep. A pending row is work not
// yet done and is never purged by age.
func (s *Store) PurgeDeliveries(now time.Time, keep time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM deliveries WHERE state != 'pending' AND updated_at < ?`, fmtTime(now.Add(-keep)))
	return err
}
```

- [ ] **Step 4b: Adjust the two lot 1 tests the new migration invalidates**

`internal/hub/store/store_test.go` asserts the schema version and enumerates the
tables. Both statements are now false, and a test that asserts yesterday's schema
is a test that will block every future migration.

```go
	if v != 2 {
		t.Fatalf("schema version = %d, want 2", v)
	}
	for _, table := range []string{"hosts", "samples", "samples_10m", "samples_1h", "samples_1d", "containers", "container_samples", "alert_rules", "alert_events", "deliveries", "users", "sessions", "settings"} {
```

`TestSchemaKeepsForeignKeys` iterates a fixed list of tables and demands each
reference `hosts`. `deliveries` references `alert_events`, so it stays out of
that list and gets its own assertion instead:

```go
func TestDeliveriesReferenceTheirEvent(t *testing.T) {
	s := openTest(t)
	var ref string
	if err := s.db.QueryRow(`SELECT "table" FROM pragma_foreign_key_list('deliveries')`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != "alert_events" {
		t.Fatalf("deliveries must reference alert_events, got %q", ref)
	}
}
```

- [ ] **Step 5: Run the tests, then the whole store package**

Run: `go test ./internal/hub/store/ -race`
Expected: PASS, including the lot 1 tests — the new migration must not disturb them.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/store
git commit -m "feat(store): deliveries table and its lifecycle"
```

---

### Task 2: The message and the channel interface

**Files:**
- Create: `internal/hub/notify/message.go`, `internal/hub/notify/channel.go`
- Test: `internal/hub/notify/message_test.go`

**Interfaces:**
- Produces:
```go
type Message struct {
    HostID int64; HostName string
    Metric string          // cpu | memory | disk | load | temperature | bandwidth | status
    Kind string            // fired | resolved
    Value float64
    Threshold float64      // 0 for the implicit status rule
    Duration time.Duration
    At time.Time
    Link string            // "" when SM_PUBLIC_URL is unset
}
func (m Message) Fired() bool
func (m Message) Title() string        // "mac-vincent memory 92% (fired)"
func (m Message) Subject() string      // "[ServersMonitor] " + Title()
func (m Message) Body() string         // plain text, a few lines, ends with Link when there is one
func (m Message) Priority() int        // ntfy: 4 for fired, 3 for resolved
func (m Message) Tags() []string       // ntfy: ["rotating_light"] / ["white_check_mark"]
func (m Message) ValueText() string    // "92%", "3.2 MB/s", "85°C", "" for status
type Channel interface { Name() string; Send(ctx context.Context, m Message) error }
type Config struct { Public string; SMTP SMTPConfig; Webhook WebhookConfig; Teams TeamsConfig }
type retryableError struct{ error }   // marks 429 / 5xx
func Retryable(err error) bool
func MarkRetryable(err error) error
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/notify/message_test.go`:
```go
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
		// midpoint, so %.1f rounds it down. Verified against the compiler, not
		// assumed from the decimal.
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
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/notify/`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write message.go**

```go
// Package notify turns alert transitions into messages and delivers them.
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
	Kind      string
	Value     float64
	Threshold float64
	Duration  time.Duration
	At        time.Time
	Link      string
}

func (m Message) Fired() bool { return m.Kind == "fired" }

// ValueText renders the value with the unit its metric is measured in.
// The implicit status rule has no value worth showing: "offline 1" means nothing.
func (m Message) ValueText() string {
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
	return fmt.Sprintf("%s %s %s (%s)", m.HostName, m.Metric, m.ValueText(), m.Kind)
}

func (m Message) Subject() string { return "[ServersMonitor] " + m.Title() }

func (m Message) Body() string {
	var b strings.Builder
	b.WriteString(m.Title())
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Host:   %s\n", m.HostName)
	if v := m.ValueText(); v != "" {
		fmt.Fprintf(&b, "Value:  %s\n", v)
		fmt.Fprintf(&b, "Rule:   above %g for %s\n", m.Threshold, shortDuration(m.Duration))
	} else {
		fmt.Fprintf(&b, "Rule:   no sample for three intervals\n")
	}
	fmt.Fprintf(&b, "Time:   %s\n", m.At.UTC().Format("2006-01-02 15:04:05 UTC"))
	if m.Link != "" {
		fmt.Fprintf(&b, "\n%s\n", m.Link)
	}
	return b.String()
}

// shortDuration renders 10m0s as 10m: the trailing zeros read as noise at 3 a.m.
// Written as arithmetic rather than TrimSuffix, because trimming "0m" off "10m"
// leaves "1" — and 10m and 30m are two of the four seeded defaults.
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
// connection refused. A 400 is not — it will fail identically forever.
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
```

- [ ] **Step 4: Write channel.go**

```go
package notify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Channel delivers one message. It knows nothing about retries or queues.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

type SMTPConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	TLSMode  string // starttls | tls | none
}

type WebhookConfig struct {
	Enabled bool
	URL     string
	Headers map[string]string
}

type TeamsConfig struct {
	Enabled bool
	URL     string
}

type Config struct {
	Public  string
	SMTP    SMTPConfig
	Webhook WebhookConfig
	Teams   TeamsConfig
}

// Link builds the host page URL, or "" when no public URL is configured: a
// wrong link is worse than none.
func (c Config) Link(hostID int64) string {
	if c.Public == "" {
		return ""
	}
	return strings.TrimSuffix(c.Public, "/") + "/hosts/" + strconv.FormatInt(hostID, 10)
}

// Validate refuses a configuration at save time rather than at 3 a.m.
func (c Config) Validate() error {
	if c.Public != "" {
		u, err := url.Parse(c.Public)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("public url must be http(s)://host")
		}
	}
	if c.SMTP.Enabled {
		if c.SMTP.Host == "" || c.SMTP.Port <= 0 || c.SMTP.From == "" || len(c.SMTP.To) == 0 {
			return fmt.Errorf("smtp needs a host, a port, a sender and at least one recipient")
		}
		switch c.SMTP.TLSMode {
		case "starttls", "tls", "none":
		default:
			return fmt.Errorf("smtp tls mode must be starttls, tls or none")
		}
	}
	if err := requireHTTPS("webhook", c.Webhook.Enabled, c.Webhook.URL, false); err != nil {
		return err
	}
	return requireHTTPS("teams", c.Teams.Enabled, c.Teams.URL, true)
}

func requireHTTPS(name string, enabled bool, raw string, httpsOnly bool) error {
	if !enabled {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s url is not a url", name)
	}
	if httpsOnly && u.Scheme != "https" {
		return fmt.Errorf("%s url must be https", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s url must be http(s)", name)
	}
	return nil
}
```

`Enabled`, which assembles the channel list, needs the three constructors and so
belongs to Task 5. Everything in `channel.go` above compiles on its own, which
keeps this task's commit buildable.

- [ ] **Step 5: Run the tests and the build**

Run: `go test ./internal/hub/notify/ -v` then `go build ./...`
Expected: PASS (5 tests), and a clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/notify
git commit -m "feat(notify): message rendering and the channel contract"
```

---

### Task 3: The SMTP channel

**Files:**
- Create: `internal/hub/notify/smtp.go`
- Test: `internal/hub/notify/smtp_test.go`

**Interfaces:**
- Consumes: `Message`, `SMTPConfig`, `MarkRetryable` (Task 2)
- Produces:
```go
type SMTP struct{ cfg SMTPConfig; dial func(addr string) (net.Conn, error) } // dial is nil in production
func NewSMTP(cfg SMTPConfig) *SMTP
func (s *SMTP) Name() string   // "smtp"
func (s *SMTP) Send(ctx context.Context, m Message) error
func BuildMail(cfg SMTPConfig, m Message, now time.Time, msgID string) []byte
```

- [ ] **Step 1: Write the failing tests**

Two levels: `BuildMail` is pure and asserts the bytes; `Send` runs against a
local fake SMTP server, which is the only way to prove the conversation itself
works without a real relay.

`internal/hub/notify/smtp_test.go`:
```go
package notify

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSMTPConfig() SMTPConfig {
	return SMTPConfig{Enabled: true, Host: "localhost", Port: 1025, From: "hub@example.test",
		To: []string{"vincent@example.test", "ops@example.test"}, TLSMode: "none"}
}

func TestBuildMailHeaders(t *testing.T) {
	raw := string(BuildMail(testSMTPConfig(), fired(), at, "abc@hub"))
	head, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("mail has no header/body separator:\n%q", raw)
	}
	for _, want := range []string{
		"From: hub@example.test",
		"To: vincent@example.test, ops@example.test",
		"Subject: [ServersMonitor] mac-vincent memory 92.4% (fired)",
		"Message-ID: <abc@hub>",
		"Date: Fri, 18 Sep 2026 03:14:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("header missing %q:\n%s", want, head)
		}
	}
	if !strings.Contains(body, "92.4%") {
		t.Errorf("body lost the value:\n%s", body)
	}
	if strings.Contains(head, "\n") != strings.Contains(head, "\r\n") {
		t.Error("headers must use CRLF line endings")
	}
}

func TestBuildMailRefusesHeaderInjection(t *testing.T) {
	// A host name is user input. If it reached the Subject line unescaped, a
	// name containing CRLF would let anyone add headers to the hub's own mail.
	//
	// The property is that no new header *line* appears. "Bcc:" surviving as
	// text inside the Subject value is harmless, and asserting its absence
	// anywhere in the block would test the wrong thing.
	m := fired()
	m.HostName = "pi\r\nBcc: attacker@example.test"
	raw := string(BuildMail(testSMTPConfig(), m, at, "x@hub"))
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if len(lines) != 8 {
		t.Fatalf("expected the 8 headers BuildMail writes, got %d:\n%s", len(lines), head)
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.ToLower(l), "bcc:") {
			t.Fatalf("header injection got through:\n%s", head)
		}
	}
	if !strings.HasPrefix(lines[2], "Subject: ") || strings.Contains(lines[2], "\n") {
		t.Fatalf("the payload must stay inside the subject line: %q", lines[2])
	}
}

// fakeSMTP is a minimal server that speaks just enough ESMTP to accept one
// message, and records what it was told.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	received []string
	reject   string // when set, the reply to DATA's payload, e.g. "451 try later"
}

func startFakeSMTP(t *testing.T, reject string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, reject: reject}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) addr() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeSMTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSMTP) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	w := func(s string) { c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var body strings.Builder
	inData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				if f.reject != "" {
					w(f.reject)
					continue
				}
				f.mu.Lock()
				f.received = append(f.received, body.String())
				f.mu.Unlock()
				body.Reset()
				w("250 ok")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			w("250-fake")
			w("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
			w("250 ok")
		case strings.HasPrefix(line, "AUTH"):
			w("235 authenticated")
		case line == "DATA":
			inData = true
			w("354 go ahead")
		case line == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func TestSMTPSendReachesTheServer(t *testing.T) {
	f := startFakeSMTP(t, "")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	ch := NewSMTP(cfg)
	if ch.Name() != "smtp" {
		t.Fatalf("name = %q", ch.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.Send(ctx, fired()); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("server received %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0], "Subject: [ServersMonitor] mac-vincent memory 92.4% (fired)") {
		t.Fatalf("wrong message:\n%s", msgs[0])
	}
}

func TestSMTPTemporaryFailureIsRetryable(t *testing.T) {
	f := startFakeSMTP(t, "451 mailbox busy")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("a 451 must be an error")
	}
	if !Retryable(err) {
		t.Fatalf("a 4xx from SMTP is temporary and must be retryable: %v", err)
	}
}

func TestSMTPPermanentFailureIsNotRetryable(t *testing.T) {
	f := startFakeSMTP(t, "550 no such user")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("a 550 must be an error")
	}
	if Retryable(err) {
		t.Fatalf("a 550 will fail identically forever: %v", err)
	}
}

func TestSMTPUnreachableHostIsRetryable(t *testing.T) {
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = "127.0.0.1", 1 // nothing listens on port 1
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a refused connection is worth retrying: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/notify/ -run SMTP -v`
Expected: FAIL, `undefined: NewSMTP`.

- [ ] **Step 3: Write smtp.go**

`net/smtp` is frozen but complete, and one dependency avoided is one dependency
to keep patched. Its `SendMail` cannot do implicit TLS on port 465, so the
conversation is written out rather than delegated.

```go
package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type SMTP struct{ cfg SMTPConfig }

func NewSMTP(cfg SMTPConfig) *SMTP { return &SMTP{cfg: cfg} }

func (s *SMTP) Name() string { return "smtp" }

// sanitizeHeader keeps a value on one line. Host names come from the browser,
// and a CRLF in one would otherwise append headers of the sender's choosing.
func sanitizeHeader(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

func newMessageID(host string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b) + "@" + host
}

// BuildMail renders one RFC 5322 message. Kept separate from Send so the bytes
// can be asserted without a server.
func BuildMail(cfg SMTPConfig, m Message, now time.Time, msgID string) []byte {
	var b strings.Builder
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, sanitizeHeader(v)) }
	h("From", cfg.From)
	h("To", strings.Join(cfg.To, ", "))
	h("Subject", m.Subject())
	h("Message-ID", "<"+msgID+">")
	h("Date", now.UTC().Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Auto-Submitted", "auto-generated") // stops well-behaved autoresponders
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(m.Body(), "\n", "\r\n"))
	return []byte(b.String())
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.cfg.TLSMode == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		// Nothing answered. That is the network, not the message.
		return MarkRetryable(fmt.Errorf("smtp dial %s: %w", addr, err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return classifySMTP(err)
	}
	defer c.Close()
	if s.cfg.TLSMode == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return classifySMTP(err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return classifySMTP(err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return classifySMTP(err)
	}
	for _, to := range s.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return classifySMTP(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return classifySMTP(err)
	}
	if _, err := w.Write(BuildMail(s.cfg, m, time.Now(), newMessageID(s.cfg.Host))); err != nil {
		return classifySMTP(err)
	}
	if err := w.Close(); err != nil { // the server's verdict on the payload arrives here
		return classifySMTP(err)
	}
	return c.Quit()
}

// classifySMTP maps a reply code onto the retry policy: 4xx is "come back
// later", 5xx is "this will never work". An error with no code at all is a
// transport problem, which is always worth another try.
func classifySMTP(err error) error {
	if code, ok := smtpCode(err); ok {
		if code >= 400 && code < 500 {
			return MarkRetryable(err)
		}
		return err
	}
	return MarkRetryable(err)
}

// smtpCode extracts the reply code from a net/textproto error, which is what
// net/smtp returns for every server refusal.
func smtpCode(err error) (int, bool) {
	var e *textproto.Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
```

Imports for `smtp.go`: `context`, `crypto/rand`, `crypto/tls`, `encoding/hex`,
`errors`, `fmt`, `net`, `net/smtp`, `net/textproto`, `strconv`, `strings`,
`time`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/notify/ -run SMTP -race -v`
Expected: PASS, 6 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/notify
git commit -m "feat(notify): smtp channel with retry classification"
```

---

### Task 4: The generic webhook channel

**Files:**
- Create: `internal/hub/notify/webhook.go`
- Test: `internal/hub/notify/webhook_test.go`

**Interfaces:**
- Consumes: `Message`, `WebhookConfig`, `MarkRetryable`
- Produces:
```go
type Webhook struct{ cfg WebhookConfig; client *http.Client }
func NewWebhook(cfg WebhookConfig) *Webhook
func (w *Webhook) Name() string  // "webhook"
func (w *Webhook) Send(ctx context.Context, m Message) error
type WebhookPayload struct { … }   // the documented JSON body
func classifyHTTP(resp *http.Response, body []byte) error
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/notify/webhook_test.go`:
```go
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookPostsTheDocumentedBody(t *testing.T) {
	var got struct {
		method, path, ctype, auth, title, ntfyTitle string
		body                                        WebhookPayload
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.ctype = r.Header.Get("Content-Type")
		got.auth = r.Header.Get("Authorization")
		got.ntfyTitle = r.Header.Get("X-Title")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got.body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL + "/hooks/sm",
		Headers: map[string]string{"Authorization": "Bearer tok"}})
	if ch.Name() != "webhook" {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(context.Background(), fired()); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/hooks/sm" {
		t.Fatalf("%s %s", got.method, got.path)
	}
	if got.ctype != "application/json" {
		t.Fatalf("content-type = %q", got.ctype)
	}
	if got.auth != "Bearer tok" {
		t.Fatalf("configured headers must be sent, got %q", got.auth)
	}
	if got.ntfyTitle != "mac-vincent memory 92.4% (fired)" {
		// ntfy reads the title from a header, not the body.
		t.Fatalf("X-Title = %q", got.ntfyTitle)
	}
	b := got.body
	if b.Host != "mac-vincent" || b.Metric != "memory" || b.Kind != "fired" || b.Value != 92.4 ||
		b.Threshold != 90 || b.Priority != 4 || b.Link != "https://hub.example/hosts/1" {
		t.Fatalf("payload = %+v", b)
	}
	if len(b.Tags) != 1 || b.Tags[0] != "rotating_light" {
		t.Fatalf("tags = %v", b.Tags)
	}
	if !strings.Contains(b.Message, "92.4%") {
		t.Fatalf("message = %q", b.Message)
	}
}

func TestWebhookRetryClassification(t *testing.T) {
	cases := []struct {
		status  int
		wantErr bool
		retry   bool
	}{
		{200, false, false},
		{204, false, false},
		{400, true, false},  // our payload is wrong; retrying changes nothing
		{404, true, false},
		{429, true, true},   // asked to slow down
		{500, true, true},
		{503, true, true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, "detail from the server")
		}))
		err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
		srv.Close()
		if (err != nil) != c.wantErr {
			t.Fatalf("status %d: err = %v, want error = %v", c.status, err, c.wantErr)
		}
		if err != nil && Retryable(err) != c.retry {
			t.Fatalf("status %d: retryable = %v, want %v", c.status, Retryable(err), c.retry)
		}
		if err != nil && !strings.Contains(err.Error(), "detail from the server") {
			t.Fatalf("status %d: the server's own words are the only clue Vincent gets: %v", c.status, err)
		}
	}
}

func TestWebhookErrorBodyIsTruncated(t *testing.T) {
	// An HTML error page must not end up whole in last_error, and from there in
	// the settings table.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("x", 10000))
	}))
	defer srv.Close()
	err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error message is %d bytes", len(err.Error()))
	}
}

func TestWebhookUnreachableIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing listens there
	err := NewWebhook(WebhookConfig{Enabled: true, URL: url}).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a dead endpoint is worth retrying: %v", err)
	}
}

func TestWebhookHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(ctx, fired()); err == nil {
		t.Fatal("a cancelled context must not send")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/notify/ -run Webhook -v`
Expected: FAIL, `undefined: NewWebhook`.

- [ ] **Step 3: Write webhook.go**

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// WebhookPayload is the contract this hub publishes. ntfy reads `message`,
// `title`, `priority` and `tags`; everything else is there for a receiver that
// wants to route on the host or the metric.
type WebhookPayload struct {
	Host      string   `json:"host"`
	HostID    int64    `json:"host_id"`
	Metric    string   `json:"metric"`
	Kind      string   `json:"kind"`
	Value     float64  `json:"value"`
	Threshold float64  `json:"threshold"`
	Title     string   `json:"title"`
	Message   string   `json:"message"`
	Priority  int      `json:"priority"`
	Tags      []string `json:"tags"`
	Link      string   `json:"link,omitempty"`
	At        string   `json:"at"`
}

type Webhook struct {
	cfg    WebhookConfig
	client *http.Client
}

func NewWebhook(cfg WebhookConfig) *Webhook {
	return &Webhook{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(WebhookPayload{
		Host: m.HostName, HostID: m.HostID, Metric: m.Metric, Kind: m.Kind,
		Value: m.Value, Threshold: m.Threshold, Title: m.Title(), Message: m.Body(),
		Priority: m.Priority(), Tags: m.Tags(), Link: m.Link,
		At: m.At.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err // a payload we cannot even encode will never encode
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ServersMonitor")
	// ntfy takes these from headers even when the body is JSON.
	req.Header.Set("X-Title", sanitizeHeader(m.Title()))
	req.Header.Set("X-Priority", strconv.Itoa(m.Priority()))
	for k, v := range w.cfg.Headers {
		req.Header.Set(k, sanitizeHeader(v))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return err // shutting down, not a channel failure
		}
		return MarkRetryable(fmt.Errorf("webhook post: %w", err))
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyHTTP(resp.StatusCode, detail)
}

// classifyHTTP is shared with Teams: 2xx is done, 429 and 5xx are worth another
// attempt, every other 4xx is a configuration problem no retry will fix.
func classifyHTTP(status int, detail []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	err := fmt.Errorf("%s: %s", http.StatusText(status), truncate(string(detail), 200))
	if status == http.StatusTooManyRequests || status >= 500 {
		return MarkRetryable(err)
	}
	return err
}

// truncate keeps an error short enough to live in a table cell and a database
// column. An HTML error page is 10 kB of nothing.
func truncate(s string, n int) string {
	s = sanitizeHeader(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/notify/ -run Webhook -race -v`
Expected: PASS, 5 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/notify
git commit -m "feat(notify): generic webhook channel"
```

---

### Task 5: The Microsoft Teams channel

**Files:**
- Create: `internal/hub/notify/teams.go`
- Test: `internal/hub/notify/teams_test.go`

**Interfaces:**
- Consumes: `Message`, `TeamsConfig`, `classifyHTTP`, `truncate`
- Produces:
```go
type Teams struct{ cfg TeamsConfig; client *http.Client }
func NewTeams(cfg TeamsConfig) *Teams
func (t *Teams) Name() string  // "teams"
func (t *Teams) Send(ctx context.Context, m Message) error
func TeamsCard(m Message) map[string]any
```

**Read before implementing:** the spec's §6 carries the payload verified against
Microsoft Learn on 2026-09-18. The connector format (`MessageCard`,
`themeColor`) is dead: Office 365 connectors were disabled between 18 and 22 May
2026. Workflow URLs live on `api.powerautomate.com`, `api.powerplatform.com` or
`flow.microsoft.com`. The message cap is 28 KB and more than 4 requests per
second is throttled with a `429`.

**Honesty about what this task proves.** A workflow URL belongs to a real
tenant, so these tests assert the payload shape byte for byte against a local
server and nothing more. They do not prove Teams accepts it. The first delivery
to a real channel is Vincent's to confirm, and Task 9 asks for it explicitly.

- [ ] **Step 1: Write the failing tests**

`internal/hub/notify/teams_test.go`:
```go
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeCard walks the Adaptive Card envelope and returns the card content.
func decodeCard(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	if env["type"] != "message" {
		t.Fatalf(`envelope type = %v, want "message"`, env["type"])
	}
	atts, ok := env["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v", env["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("contentType = %v", att["contentType"])
	}
	if v, present := att["contentUrl"]; !present || v != nil {
		t.Fatalf("contentUrl must be present and null, got %v (present=%v)", v, present)
	}
	return att["content"].(map[string]any)
}

func TestTeamsCardEnvelope(t *testing.T) {
	raw, err := json.Marshal(TeamsCard(fired()))
	if err != nil {
		t.Fatal(err)
	}
	card := decodeCard(t, raw)
	if card["$schema"] != "http://adaptivecards.io/schemas/adaptive-card.json" {
		t.Fatalf("$schema = %v", card["$schema"])
	}
	if card["type"] != "AdaptiveCard" {
		t.Fatalf("card type = %v", card["type"])
	}
	if card["version"] != "1.2" {
		// 1.2 is what the workflow connector guarantees; a higher version
		// renders as a blank card on some clients.
		t.Fatalf("version = %v", card["version"])
	}
	body, _ := card["body"].([]any)
	if len(body) == 0 {
		t.Fatal("card has no body")
	}
	flat, _ := json.Marshal(body)
	for _, want := range []string{"mac-vincent", "92.4%", "memory"} {
		if !strings.Contains(string(flat), want) {
			t.Fatalf("card body is missing %q: %s", want, flat)
		}
	}
}

func TestTeamsCardLinksWhenThereIsALink(t *testing.T) {
	raw, _ := json.Marshal(TeamsCard(fired()))
	if !strings.Contains(string(raw), "https://hub.example/hosts/1") {
		t.Fatalf("a configured link must reach the card: %s", raw)
	}
	m := fired()
	m.Link = ""
	raw, _ = json.Marshal(TeamsCard(m))
	var env map[string]any
	json.Unmarshal(raw, &env)
	card := decodeCard(t, raw)
	if _, has := card["actions"]; has {
		t.Fatalf("no link configured means no Open button: %s", raw)
	}
}

func TestTeamsCardStaysUnderTheSizeCap(t *testing.T) {
	// Teams rejects a message above 28 KB.
	m := fired()
	m.HostName = strings.Repeat("long-host-name-", 500)
	raw, _ := json.Marshal(TeamsCard(m))
	if len(raw) > 28*1024 {
		t.Fatalf("card is %d bytes, over the 28 KB cap", len(raw))
	}
}

func TestTeamsSendPostsTheCard(t *testing.T) {
	var body []byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted) // what a workflow actually answers
	}))
	defer srv.Close()
	ch := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL})
	if ch.Name() != "teams" {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(context.Background(), fired()); err != nil {
		t.Fatalf("a 202 is success: %v", err)
	}
	if ctype != "application/json" {
		t.Fatalf("content-type = %q", ctype)
	}
	decodeCard(t, body)
}

func TestTeamsThrottlingIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "rate limit")
	}))
	defer srv.Close()
	err := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a 429 from Teams must be retried: %v", err)
	}
}

func TestTeamsExpiredWorkflowIsNotRetryable(t *testing.T) {
	// A deleted or expired workflow answers 4xx. Retrying it four times a minute
	// for every alert helps nobody.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if err == nil || Retryable(err) {
		t.Fatalf("401 = %v (retryable %v)", err, Retryable(err))
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/notify/ -run Teams -v`
Expected: FAIL, `undefined: TeamsCard`.

- [ ] **Step 3: Write teams.go**

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// teamsMaxHostName bounds the one field that comes from user input, so a long
// name cannot push the card past the 28 KB Teams accepts.
const teamsMaxHostName = 200

// TeamsCard builds the Adaptive Card envelope a Power Automate workflow expects.
// The connector format (MessageCard, themeColor) was disabled in May 2026 and
// must not come back.
func TeamsCard(m Message) map[string]any {
	title := "🔴 " + m.Title()
	if !m.Fired() {
		title = "🟢 " + m.Title()
	}
	facts := []map[string]any{
		{"title": "Host", "value": truncate(m.HostName, teamsMaxHostName)},
		{"title": "Time", "value": m.At.UTC().Format("2006-01-02 15:04:05 UTC")},
	}
	if v := m.ValueText(); v != "" {
		facts = append(facts,
			map[string]any{"title": "Value", "value": v},
			map[string]any{"title": "Rule", "value": fmt.Sprintf("above %g for %s", m.Threshold, shortDuration(m.Duration))})
	} else {
		facts = append(facts, map[string]any{"title": "Rule", "value": "no sample for three intervals"})
	}
	content := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.2",
		"body": []any{
			map[string]any{"type": "TextBlock", "text": truncate(title, teamsMaxHostName+80),
				"weight": "Bolder", "size": "Medium", "wrap": true},
			map[string]any{"type": "FactSet", "facts": facts},
		},
	}
	if m.Link != "" {
		content["actions"] = []any{
			map[string]any{"type": "Action.OpenUrl", "title": "Open in ServersMonitor", "url": m.Link},
		}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{
			map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"contentUrl":  nil, // required by the workflow connector, and it must be null
				"content":     content,
			},
		},
	}
}

type Teams struct {
	cfg    TeamsConfig
	client *http.Client
}

func NewTeams(cfg TeamsConfig) *Teams {
	return &Teams{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
}

func (t *Teams) Name() string { return "teams" }

func (t *Teams) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(TeamsCard(m))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ServersMonitor")
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return MarkRetryable(fmt.Errorf("teams post: %w", err))
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyHTTP(resp.StatusCode, detail)
}
```

- [ ] **Step 4: Add `Enabled` to `channel.go`**

All three constructors now exist, so the function that assembles them can be
written. Append to `internal/hub/notify/channel.go`:

```go
// Enabled builds the channels the configuration asks for, in a stable order.
func Enabled(c Config) []Channel {
	var out []Channel
	if c.SMTP.Enabled {
		out = append(out, NewSMTP(c.SMTP))
	}
	if c.Webhook.Enabled {
		out = append(out, NewWebhook(c.Webhook))
	}
	if c.Teams.Enabled {
		out = append(out, NewTeams(c.Teams))
	}
	return out
}
```

- [ ] **Step 5: Run the tests, and the whole package**

Run: `go test ./internal/hub/notify/ -race -v` then `go build ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/notify
git commit -m "feat(notify): teams adaptive card channel"
```

---

### Task 6: The dispatcher

**Files:**
- Create: `internal/hub/notify/dispatcher.go`
- Test: `internal/hub/notify/dispatcher_test.go`

**Interfaces:**
- Consumes: `Channel`, `Message`, `Retryable` (Tasks 2-5)
- Produces:
```go
// Recorder is the slice of the store the dispatcher needs. Keeping it an
// interface lets the tests count state changes without a database.
type Recorder interface {
    MarkDeliverySent(id int64, now time.Time, attempts int) error
    MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error
}
type Job struct { DeliveryID int64; Channel Channel; Message Message }
type Dispatcher struct { … }
type Options struct {
    Queue int                  // default 256
    Attempts int               // default 4
    Base time.Duration         // default 1s
    Factor int                 // default 5
    Sleep func(context.Context, time.Duration) bool // nil means real time
    Now func() time.Time
    Log *slog.Logger
}
func NewDispatcher(rec Recorder, opt Options) *Dispatcher
func (d *Dispatcher) Start(ctx context.Context)   // one worker goroutine
func (d *Dispatcher) Enqueue(jobs ...Job)         // never blocks
func (d *Dispatcher) Wait()                       // tests only: worker drained and stopped
func (d *Dispatcher) Backoff(attempt int) time.Duration
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/notify/dispatcher_test.go`:
```go
package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder records what the dispatcher decided, which is the whole contract.
type recorder struct {
	mu     sync.Mutex
	sent   map[int64]int    // delivery id -> attempts
	failed map[int64]string // delivery id -> reason
	done   chan struct{}
	want   int
	seen   int
}

func newRecorder(want int) *recorder {
	return &recorder{sent: map[int64]int{}, failed: map[int64]string{}, done: make(chan struct{}), want: want}
}

func (r *recorder) settle() {
	r.seen++
	if r.seen == r.want {
		close(r.done)
	}
}

func (r *recorder) MarkDeliverySent(id int64, _ time.Time, attempts int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent[id] = attempts
	r.settle()
	return nil
}

func (r *recorder) MarkDeliveryFailed(id int64, _ time.Time, attempts int, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed[id] = reason
	r.settle()
	return nil
}

func (r *recorder) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.mu.Lock()
		defer r.mu.Unlock()
		t.Fatalf("timed out: %d of %d settled (sent=%v failed=%v)", r.seen, r.want, r.sent, r.failed)
	}
}

// fakeChannel answers from a script: one entry per attempt, reused once exhausted.
type fakeChannel struct {
	name    string
	script  []error
	calls   atomic.Int32
	seen    chan Message
}

func newFake(name string, script ...error) *fakeChannel {
	return &fakeChannel{name: name, script: script, seen: make(chan Message, 32)}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Send(ctx context.Context, m Message) error {
	n := int(f.calls.Add(1)) - 1
	f.seen <- m
	if n < len(f.script) {
		return f.script[n]
	}
	if len(f.script) == 0 {
		return nil
	}
	return f.script[len(f.script)-1]
}

// noSleep makes the retry schedule instantaneous while still being observed.
func testOptions(slept *[]time.Duration, mu *sync.Mutex) Options {
	return Options{Sleep: func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		*slept = append(*slept, d)
		mu.Unlock()
		return true
	}}
}

func TestDispatcherSendsOnceOnSuccess(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("smtp")
	d.Enqueue(Job{DeliveryID: 7, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("channel called %d times", got)
	}
	if rec.sent[7] != 1 {
		t.Fatalf("sent = %v", rec.sent)
	}
	if len(slept) != 0 {
		t.Fatalf("a success must not sleep: %v", slept)
	}
	if m := <-ch.seen; m.HostName != "mac-vincent" {
		t.Fatalf("message = %+v", m)
	}
}

func TestDispatcherRetriesRetryableFailures(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("webhook", MarkRetryable(errors.New("503")), MarkRetryable(errors.New("503")), nil)
	d.Enqueue(Job{DeliveryID: 9, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 3 {
		t.Fatalf("channel called %d times, want 3", got)
	}
	if rec.sent[9] != 3 {
		t.Fatalf("the recorded attempt count must be the real one: %v", rec.sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 5*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s]", slept)
	}
}

func TestDispatcherGivesUpAfterFourAttempts(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("teams", MarkRetryable(errors.New("boom")))
	d.Enqueue(Job{DeliveryID: 11, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 4 {
		t.Fatalf("channel called %d times, want 4", got)
	}
	if reason := rec.failed[11]; reason != "boom" {
		t.Fatalf("failure reason = %q", reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 3 || slept[2] != 25*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s 25s]", slept)
	}
}

func TestDispatcherDoesNotRetryPermanentFailures(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("webhook", errors.New("Bad Request: metric unknown"))
	d.Enqueue(Job{DeliveryID: 13, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("a 400 must be tried once, was tried %d times", got)
	}
	if rec.failed[13] != "Bad Request: metric unknown" {
		t.Fatalf("failed = %v", rec.failed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 0 {
		t.Fatalf("no sleeping between attempts that never happen: %v", slept)
	}
}

func TestEnqueueNeverBlocks(t *testing.T) {
	// The evaluator calls Enqueue on the hub's own goroutine. If it ever blocked,
	// a dead webhook would stop the hub from evaluating alerts at all.
	rec := newRecorder(2)
	block := make(chan struct{})
	slow := &blockingChannel{release: block}
	d := NewDispatcher(rec, Options{Queue: 1, Sleep: func(context.Context, time.Duration) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	done := make(chan struct{})
	go func() {
		for i := int64(0); i < 200; i++ {
			d.Enqueue(Job{DeliveryID: i, Channel: slow, Message: fired()})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Enqueue blocked")
	}
	close(block)
}

type blockingChannel struct{ release chan struct{} }

func (b *blockingChannel) Name() string { return "slow" }
func (b *blockingChannel) Send(ctx context.Context, m Message) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func TestQueueFullDropsTheOldestFiredAndKeepsResolved(t *testing.T) {
	// A dropped alert is bad. A dropped recovery is worse: the last thing
	// Vincent ever heard about that host is that it broke.
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Queue: 2, Sleep: func(context.Context, time.Duration) bool { return true }})
	ch := newFake("smtp")
	resolved := fired()
	resolved.Kind = "resolved"
	// No worker started: the queue fills and stays full.
	d.Enqueue(Job{DeliveryID: 1, Channel: ch, Message: fired()})
	d.Enqueue(Job{DeliveryID: 2, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 3, Channel: ch, Message: fired()})
	ids := d.queued()
	if len(ids) != 2 {
		t.Fatalf("queue = %v, want 2 entries", ids)
	}
	if ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("queue = %v, want the resolved (2) kept and the oldest fired (1) dropped", ids)
	}
	if reason := rec.failed[1]; reason != "queue full" {
		t.Fatalf("a dropped delivery must be recorded as failed, got %q", reason)
	}
}

func TestQueueFullDropsAResolvedOnlyAsALastResort(t *testing.T) {
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Queue: 2, Sleep: func(context.Context, time.Duration) bool { return true }})
	ch := newFake("smtp")
	resolved := fired()
	resolved.Kind = "resolved"
	d.Enqueue(Job{DeliveryID: 1, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 2, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 3, Channel: ch, Message: resolved})
	ids := d.queued()
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("queue = %v, want the oldest resolved dropped when nothing else can be", ids)
	}
	if rec.failed[1] != "queue full" {
		t.Fatalf("failed = %v", rec.failed)
	}
}

func TestShutdownStopsRetrying(t *testing.T) {
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Sleep: func(ctx context.Context, _ time.Duration) bool {
		return ctx.Err() == nil // a real sleep returns false when cancelled
	}})
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	ch := newFake("teams", MarkRetryable(errors.New("down")))
	d.Enqueue(Job{DeliveryID: 5, Channel: ch, Message: fired()})
	<-ch.seen // the first attempt happened
	cancel()
	d.Wait()
	if got := ch.calls.Load(); got > 2 {
		t.Fatalf("a cancelled dispatcher kept retrying: %d attempts", got)
	}
}

func TestBackoffSchedule(t *testing.T) {
	d := NewDispatcher(newRecorder(0), Options{})
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 5 * time.Second, 3: 25 * time.Second} {
		if got := d.Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/notify/ -run "Dispatcher|Queue|Enqueue|Shutdown|Backoff" -v`
Expected: FAIL, `undefined: NewDispatcher`.

- [ ] **Step 3: Write dispatcher.go**

The queue is a slice behind a mutex rather than a buffered channel, because the
drop policy has to inspect what is already queued and a channel cannot be
inspected.

```go
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Recorder is the part of the store the dispatcher touches. The delivery row
// already exists as pending when a job arrives; the worker only settles it.
type Recorder interface {
	MarkDeliverySent(id int64, now time.Time, attempts int) error
	MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error
}

type Job struct {
	DeliveryID int64
	Channel    Channel
	Message    Message
}

type Options struct {
	Queue    int
	Attempts int
	Base     time.Duration
	Factor   int
	// Sleep waits d and reports whether the wait completed. Tests replace it to
	// make the schedule instantaneous and observable.
	Sleep func(ctx context.Context, d time.Duration) bool
	Now   func() time.Time
	Log   *slog.Logger
}

type Dispatcher struct {
	rec  Recorder
	opt  Options
	mu   sync.Mutex
	jobs []Job
	wake chan struct{}
	done chan struct{}
}

func NewDispatcher(rec Recorder, opt Options) *Dispatcher {
	if opt.Queue <= 0 {
		opt.Queue = 256
	}
	if opt.Attempts <= 0 {
		opt.Attempts = 4
	}
	if opt.Base <= 0 {
		opt.Base = time.Second
	}
	if opt.Factor <= 0 {
		opt.Factor = 5
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	if opt.Sleep == nil {
		opt.Sleep = sleepCtx
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Dispatcher{rec: rec, opt: opt, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// sleepCtx waits d, or returns false as soon as ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Backoff is the wait before attempt+1: 1 s, 5 s, 25 s.
func (d *Dispatcher) Backoff(attempt int) time.Duration {
	w := d.opt.Base
	for i := 1; i < attempt; i++ {
		w *= time.Duration(d.opt.Factor)
	}
	return w
}

// Enqueue never blocks. When the queue is full it drops the oldest job it can
// afford to lose — a fired before a resolved — and records that drop as a
// failed delivery, so the event log never claims a delivery that never left.
func (d *Dispatcher) Enqueue(jobs ...Job) {
	d.mu.Lock()
	var dropped []Job
	for _, j := range jobs {
		if len(d.jobs) >= d.opt.Queue {
			idx := d.victim()
			dropped = append(dropped, d.jobs[idx])
			d.jobs = append(d.jobs[:idx], d.jobs[idx+1:]...)
		}
		d.jobs = append(d.jobs, j)
	}
	d.mu.Unlock()
	for _, j := range dropped {
		d.opt.Log.Warn("notification queue full, delivery dropped",
			"channel", j.Channel.Name(), "host", j.Message.HostName, "kind", j.Message.Kind)
		if err := d.rec.MarkDeliveryFailed(j.DeliveryID, d.opt.Now(), 0, "queue full"); err != nil {
			d.opt.Log.Error("record dropped delivery", "err", err)
		}
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// victim picks the oldest fired job, or the oldest job of any kind when every
// queued job is a recovery. Caller holds the lock.
func (d *Dispatcher) victim() int {
	for i, j := range d.jobs {
		if j.Message.Fired() {
			return i
		}
	}
	return 0
}

// queued returns the delivery ids currently waiting, oldest first. Tests only.
func (d *Dispatcher) queued() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]int64, len(d.jobs))
	for i, j := range d.jobs {
		out[i] = j.DeliveryID
	}
	return out
}

func (d *Dispatcher) pop() (Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.jobs) == 0 {
		return Job{}, false
	}
	j := d.jobs[0]
	d.jobs = d.jobs[1:]
	return j, true
}

// Start runs the single worker until ctx is cancelled.
func (d *Dispatcher) Start(ctx context.Context) {
	go func() {
		defer close(d.done)
		for {
			j, ok := d.pop()
			if !ok {
				select {
				case <-ctx.Done():
					return
				case <-d.wake:
					continue
				}
			}
			d.deliver(ctx, j)
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

// Wait blocks until the worker has stopped. Tests only.
func (d *Dispatcher) Wait() { <-d.done }

func (d *Dispatcher) deliver(ctx context.Context, j Job) {
	var last error
	for attempt := 1; attempt <= d.opt.Attempts; attempt++ {
		err := j.Channel.Send(ctx, j.Message)
		if err == nil {
			if err := d.rec.MarkDeliverySent(j.DeliveryID, d.opt.Now(), attempt); err != nil {
				d.opt.Log.Error("record sent delivery", "err", err)
			}
			return
		}
		last = err
		if !Retryable(err) || attempt == d.opt.Attempts {
			break
		}
		if !d.opt.Sleep(ctx, d.Backoff(attempt)) {
			return // shutting down; the row stays pending and is retried next boot
		}
	}
	d.opt.Log.Warn("notification failed", "channel", j.Channel.Name(),
		"host", j.Message.HostName, "kind", j.Message.Kind, "err", last)
	if err := d.rec.MarkDeliveryFailed(j.DeliveryID, d.opt.Now(), d.attemptsUsed(last), truncate(last.Error(), 200)); err != nil {
		d.opt.Log.Error("record failed delivery", "err", err)
	}
}

// attemptsUsed is 1 for a permanent failure, the full budget otherwise.
func (d *Dispatcher) attemptsUsed(err error) int {
	if Retryable(err) {
		return d.opt.Attempts
	}
	return 1
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/notify/ -race -v`
Expected: PASS, the whole package.

Note on `TestShutdownStopsRetrying`: `deliver` returns on a cancelled sleep
without recording anything, which is deliberate. The row stays `pending` and
Task 7 replays it on the next boot.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/notify
git commit -m "feat(notify): dispatcher with retry, backoff and a drop policy"
```

---

### Task 7: Wiring the hub

**Files:**
- Modify: `internal/hub/hub.go`
- Create: `internal/hub/notify/config_store.go`
- Test: `internal/hub/notify_test.go`

**Interfaces:**
- Consumes: everything above
- Produces:
```go
// in package notify
func LoadConfig(g Getter) Config          // reads the settings rows
func SaveConfig(s Setter, c Config) error
type Getter interface{ GetSetting(key string) (string, bool, error) }
type Setter interface{ SetSetting(key, value string) error }
// in package hub
func (h *Hub) notify(events []store.AlertEvent, hosts map[int64]store.Host, rules []store.Rule)
func (h *Hub) replayPendingDeliveries()
```

Settings keys, all under the `notify_` prefix:
`notify_public_url`, `notify_smtp_enabled`, `notify_smtp_host`,
`notify_smtp_port`, `notify_smtp_username`, `notify_smtp_password`,
`notify_smtp_from`, `notify_smtp_to` (comma separated), `notify_smtp_tls`,
`notify_webhook_enabled`, `notify_webhook_url`, `notify_webhook_headers` (JSON
object), `notify_teams_enabled`, `notify_teams_url`.

- [ ] **Step 1: Write the failing tests**

`internal/hub/notify/config_store_test.go`:
```go
package notify

import (
	"reflect"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(k string) (string, bool, error) { v, ok := f[k]; return v, ok, nil }
func (f fakeSettings) SetSetting(k, v string) error              { f[k] = v; return nil }

func TestConfigRoundTrip(t *testing.T) {
	want := Config{
		Public: "https://hub.example",
		SMTP: SMTPConfig{Enabled: true, Host: "smtp.example", Port: 587, Username: "u", Password: "p",
			From: "hub@example", To: []string{"a@example", "b@example"}, TLSMode: "starttls"},
		Webhook: WebhookConfig{Enabled: true, URL: "https://ntfy.example/sm", Headers: map[string]string{"Authorization": "Bearer t"}},
		Teams:   TeamsConfig{Enabled: true, URL: "https://api.powerautomate.com/x"},
	}
	st := fakeSettings{}
	if err := SaveConfig(st, want); err != nil {
		t.Fatal(err)
	}
	got := LoadConfig(st)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadConfigOnAnEmptyStoreIsAllDisabled(t *testing.T) {
	c := LoadConfig(fakeSettings{})
	if c.SMTP.Enabled || c.Webhook.Enabled || c.Teams.Enabled {
		t.Fatalf("a fresh install notifies nobody: %+v", c)
	}
	if len(Enabled(c)) != 0 {
		t.Fatal("no channel must be built")
	}
	if c.SMTP.TLSMode != "starttls" {
		t.Fatalf("the default must be the safe one, got %q", c.SMTP.TLSMode)
	}
}

func TestLoadConfigSurvivesGarbage(t *testing.T) {
	// Settings are text. A hand-edited database must not crash the hub.
	st := fakeSettings{"notify_smtp_port": "not-a-number", "notify_webhook_headers": "{[", "notify_smtp_to": " , a@example , "}
	c := LoadConfig(st)
	if c.SMTP.Port != 587 {
		t.Fatalf("port = %d, want the 587 default", c.SMTP.Port)
	}
	if c.Webhook.Headers == nil || len(c.Webhook.Headers) != 0 {
		t.Fatalf("headers = %v, want an empty map", c.Webhook.Headers)
	}
	if !reflect.DeepEqual(c.SMTP.To, []string{"a@example"}) {
		t.Fatalf("recipients = %q, blanks must be dropped", c.SMTP.To)
	}
}

func TestValidateRejectsWhatWouldFailAtThreeInTheMorning(t *testing.T) {
	cases := map[string]Config{
		"smtp without recipients": {SMTP: SMTPConfig{Enabled: true, Host: "h", Port: 25, From: "f", TLSMode: "none"}},
		"smtp without host":       {SMTP: SMTPConfig{Enabled: true, Port: 25, From: "f", To: []string{"a"}, TLSMode: "none"}},
		"bad tls mode":            {SMTP: SMTPConfig{Enabled: true, Host: "h", Port: 25, From: "f", To: []string{"a"}, TLSMode: "ssl"}},
		"teams over http":         {Teams: TeamsConfig{Enabled: true, URL: "http://example/x"}},
		"webhook not a url":       {Webhook: WebhookConfig{Enabled: true, URL: "not a url"}},
		"public url not a url":    {Public: "hub.example"},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	ok := Config{Public: "https://hub.example", Teams: TeamsConfig{Enabled: true, URL: "https://api.powerautomate.com/x"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid config was rejected: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("the empty config is valid: %v", err)
	}
}

func TestLinkIsEmptyWithoutAPublicURL(t *testing.T) {
	if got := (Config{}).Link(3); got != "" {
		t.Fatalf("link = %q, want empty: a wrong link is worse than none", got)
	}
	if got := (Config{Public: "https://hub.example/"}).Link(3); got != "https://hub.example/hosts/3" {
		t.Fatalf("link = %q", got)
	}
}
```

`internal/hub/notify_test.go` (package `hub`) proves the wiring, which is the
part no unit test above can reach:
```go
package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// catcher records every webhook POST it receives.
type catcher struct {
	mu   sync.Mutex
	got  []notify.WebhookPayload
	hit  chan struct{}
	srv  *httptest.Server
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
	ev := store.AlertEvent{RuleID: 0, HostID: host.ID, Metric: "status", Kind: "fired", Value: 1, At: time.Now().UTC()}
	saved, err := h.st.InsertAlertEvent(ev)
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
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu", Kind: "fired", Value: 99, At: time.Now().UTC()})
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
	saved, _ := h1.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu", Kind: "fired", Value: 99, At: time.Now().UTC()})
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
	saved, _ := h.st.InsertAlertEvent(store.AlertEvent{HostID: host.ID, Metric: "cpu", Kind: "fired", Value: 99, At: time.Now().UTC()})
	d, _ := h.st.CreateDelivery(saved.ID, "webhook", time.Now().UTC())
	h.st.DeleteHost(host.ID)
	h.replayPendingDeliveries() // must not panic and must not enqueue
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
```

Add the two helpers at the bottom of `internal/hub/notify_test.go`:
```go
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
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/... -run "Config|Notify|Alert|Pending|Delivery" -v`
Expected: FAIL, `undefined: SaveConfig`, `h.dispatch undefined`.

- [ ] **Step 3: Write config_store.go**

```go
package notify

import (
	"encoding/json"
	"strconv"
	"strings"
)

type Getter interface {
	GetSetting(key string) (string, bool, error)
}

type Setter interface {
	SetSetting(key, value string) error
}

func str(g Getter, key, def string) string {
	v, ok, err := g.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v
}

func boolean(g Getter, key string) bool { return str(g, key, "") == "true" }

func integer(g Getter, key string, def int) int {
	n, err := strconv.Atoi(str(g, key, ""))
	if err != nil {
		return def
	}
	return n
}

// LoadConfig never fails: a hub that cannot parse its own settings still has to
// boot and still has to monitor. Bad values fall back to the defaults.
func LoadConfig(g Getter) Config {
	headers := map[string]string{}
	if raw := str(g, "notify_webhook_headers", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &headers); err != nil {
			headers = map[string]string{}
		}
	}
	return Config{
		Public: str(g, "notify_public_url", ""),
		SMTP: SMTPConfig{
			Enabled:  boolean(g, "notify_smtp_enabled"),
			Host:     str(g, "notify_smtp_host", ""),
			Port:     integer(g, "notify_smtp_port", 587),
			Username: str(g, "notify_smtp_username", ""),
			Password: str(g, "notify_smtp_password", ""),
			From:     str(g, "notify_smtp_from", ""),
			To:       splitList(str(g, "notify_smtp_to", "")),
			TLSMode:  str(g, "notify_smtp_tls", "starttls"),
		},
		Webhook: WebhookConfig{
			Enabled: boolean(g, "notify_webhook_enabled"),
			URL:     str(g, "notify_webhook_url", ""),
			Headers: headers,
		},
		Teams: TeamsConfig{
			Enabled: boolean(g, "notify_teams_enabled"),
			URL:     str(g, "notify_teams_url", ""),
		},
	}
}

// splitList drops blanks, so a trailing comma does not become a recipient the
// SMTP server rejects for the whole message.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func SaveConfig(s Setter, c Config) error {
	headers, err := json.Marshal(c.Webhook.Headers)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{
		"notify_public_url":      c.Public,
		"notify_smtp_enabled":    strconv.FormatBool(c.SMTP.Enabled),
		"notify_smtp_host":       c.SMTP.Host,
		"notify_smtp_port":       strconv.Itoa(c.SMTP.Port),
		"notify_smtp_username":   c.SMTP.Username,
		"notify_smtp_password":   c.SMTP.Password,
		"notify_smtp_from":       c.SMTP.From,
		"notify_smtp_to":         strings.Join(c.SMTP.To, ","),
		"notify_smtp_tls":        c.SMTP.TLSMode,
		"notify_webhook_enabled": strconv.FormatBool(c.Webhook.Enabled),
		"notify_webhook_url":     c.Webhook.URL,
		"notify_webhook_headers": string(headers),
		"notify_teams_enabled":   strconv.FormatBool(c.Teams.Enabled),
		"notify_teams_url":       c.Teams.URL,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}
```

`LoadConfig` on an empty store must return `Headers` as an empty map, not nil,
for the round-trip test to hold. `SaveConfig` of a nil map writes `null`, which
unmarshals into the empty map — which is why the initialisation above happens
before the parse.

- [ ] **Step 4: Wire the hub**

In `internal/hub/hub.go`, add to the `Hub` struct:
```go
	dispatch *notify.Dispatcher
	ncfg     atomic.Pointer[notify.Config]
	channels atomic.Pointer[[]notify.Channel]
```

An atomic pointer rather than a mutex: the settings handler swaps the whole
configuration, the evaluator only ever reads it.

In `New`, after the machine is restored:
```go
	h := &Hub{cfg: cfg, log: log, st: st, bus: bus, agents: agents, machine: machine}
	h.dispatch = notify.NewDispatcher(st, notify.Options{Log: log.With("component", "notify")})
	h.ReloadNotify()
```

Add the three methods:
```go
// ReloadNotify rebuilds the channel list from the settings. Called at boot and
// whenever the notifications settings are saved. Exported because the server
// reaches it through the Notifier interface of Task 8.
func (h *Hub) ReloadNotify() {
	c := notify.LoadConfig(h.st)
	chans := notify.Enabled(c)
	h.ncfg.Store(&c)
	h.channels.Store(&chans)
}

// notify records one pending delivery per enabled channel, then hands the work
// to the dispatcher. The row is written first: a crash between here and the
// send leaves evidence, not silence.
func (h *Hub) notify(events []store.AlertEvent) {
	chans := *h.channels.Load()
	if len(chans) == 0 || len(events) == 0 {
		return
	}
	cfg := *h.ncfg.Load()
	now := time.Now().UTC()
	var jobs []notify.Job
	for _, e := range events {
		host, err := h.st.Host(e.HostID)
		if err != nil {
			h.log.Error("notify: host", "err", err, "host_id", e.HostID)
			continue
		}
		m := h.message(cfg, e, host)
		for _, ch := range chans {
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

// message turns a stored event into what a channel renders, looking up the
// threshold of the rule that fired. A rule deleted since keeps a zero
// threshold rather than blocking the notification.
func (h *Hub) message(cfg notify.Config, e store.AlertEvent, host store.Host) notify.Message {
	m := notify.Message{HostID: host.ID, HostName: host.Name, Metric: e.Metric,
		Kind: e.Kind, Value: e.Value, At: e.At, Link: cfg.Link(host.ID)}
	if e.RuleID != alerts.StatusRuleID {
		if rules, err := h.st.ListRules(); err == nil {
			for _, r := range rules {
				if r.ID == e.RuleID {
					m.Threshold, m.Duration = r.Threshold, r.Duration
					break
				}
			}
		}
	}
	return m
}

// replayPendingDeliveries picks up what a previous run left unfinished.
//
// Delivery is at least once, deliberately. The pending row is written before
// the first attempt, so a crash between a channel's 200 and MarkDeliverySent
// replays the message on the next boot and Vincent gets it twice. A duplicate
// alert is a nuisance; a lost one is the failure this whole lot exists to
// prevent. The README says so, so a second email is not read as a bug.
func (h *Hub) replayPendingDeliveries() {
	pending, err := h.st.PendingDeliveries()
	if err != nil {
		h.log.Error("replay deliveries", "err", err)
		return
	}
	chans := *h.channels.Load()
	byName := map[string]notify.Channel{}
	for _, c := range chans {
		byName[c.Name()] = c
	}
	cfg := *h.ncfg.Load()
	var jobs []notify.Job
	for _, d := range pending {
		ch, ok := byName[d.Channel]
		if !ok {
			// The channel was turned off while this was queued. Nothing can
			// deliver it, and pretending otherwise would keep it pending forever.
			h.st.MarkDeliveryFailed(d.ID, time.Now().UTC(), 0, "channel disabled")
			continue
		}
		e, host, err := h.st.DeliveryEvent(d.ID)
		if err != nil {
			continue // the event is gone, and so is its delivery, by cascade
		}
		jobs = append(jobs, notify.Job{DeliveryID: d.ID, Channel: ch, Message: h.message(cfg, e, host)})
	}
	if len(jobs) > 0 {
		h.log.Info("replaying deliveries left by a previous run", "count", len(jobs))
		h.dispatch.Enqueue(jobs...)
	}
}
```

In `evaluate`, collect what was persisted and notify once at the end. Replace
the loop body's end so that:
```go
	var saved []store.AlertEvent
	for _, e := range events {
		stored, err := h.st.InsertAlertEvent(e)
		if err != nil {
			h.log.Error("insert alert event", "err", err)
			continue
		}
		saved = append(saved, stored)
		h.log.Info("alert", "kind", e.Kind, "metric", e.Metric, "host_id", e.HostID, "value", e.Value)
		h.bus.Publish("alert", map[string]any{"host_id": e.HostID, "metric": e.Metric, "kind": e.Kind, "value": e.Value})
	}
	h.notify(saved)
```

In `Run`, start the dispatcher and replay before the first tick:
```go
func (h *Hub) Run(ctx context.Context) error {
	h.dispatch.Start(ctx)
	h.replayPendingDeliveries()
	fast := time.NewTicker(h.interval())
	…
```

In `hourly`, purge settled deliveries after 30 days:
```go
	if err := h.st.PurgeDeliveries(now, 30*24*time.Hour); err != nil {
		h.log.Error("purge deliveries", "err", err)
	}
```

New imports in `hub.go`: `sync/atomic` and
`github.com/vincentlauriat/serversmonitor/internal/hub/notify`.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/hub/... -race`
Expected: PASS. Then `go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add internal/hub
git commit -m "feat(hub): record and dispatch a delivery per alert transition"
```

---

### Task 8: The notifications API

**Files:**
- Create: `internal/hub/server/api_notify.go`
- Modify: `internal/hub/server/server.go` (routes and `Deps`)
- Test: `internal/hub/server/api_notify_test.go`

**Interfaces:**
- Consumes: `notify.Config`, `notify.LoadConfig`, `notify.SaveConfig`, `notify.Enabled`, `store.ChannelHealth`, `store.ListDeliveries`
- Produces:
```go
// Notifier is what the server needs from the hub: reload after a save, and send
// a test message on demand. Keeping it an interface avoids a server→hub import.
type Notifier interface {
    ReloadNotify()
    TestNotify(ctx context.Context, channel string) error
}
// routes
// GET    /api/v1/notifications              -> notifyView
// PUT    /api/v1/notifications              -> 204
// POST   /api/v1/notifications/test         -> 204 or 502 with the real reason
// GET    /api/v1/notifications/deliveries   -> []deliveryView
```

The password rule, from §7 of the spec: a read never returns it, only
`smtp_password_set`. On a write, a field absent from the JSON means *leave it
alone*; an empty string means *clear it*. Pointers make that distinction
expressible, which a plain string cannot.

- [ ] **Step 1: Write the failing tests**

The package already has a test harness called `rig` (`newRig`, `r.do`,
`r.setupAndLogin`, `r.newHost`, `r.st`, `r.now`). Use it. Do not add a second
way to build a test server.

`internal/hub/server/api_notify_test.go`:
```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type fakeNotifier struct {
	reloads int
	tested  []string
	err     error
}

func (f *fakeNotifier) ReloadNotify() { f.reloads++ }

func (f *fakeNotifier) TestNotify(_ context.Context, channel string) error {
	f.tested = append(f.tested, channel)
	return f.err
}

// notifyRig is a rig whose notifier can be inspected. newRig is extended in
// step 3 to build one and hang it on the rig, so every existing test keeps
// working unchanged.
func newNotifyRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.setupAndLogin(t)
	return r
}

func (r *rig) notifier(t *testing.T) *fakeNotifier {
	t.Helper()
	f, ok := r.notify.(*fakeNotifier)
	if !ok {
		t.Fatalf("rig notifier is %T", r.notify)
	}
	return f
}

func TestNotificationsDefaultsAreAllOff(t *testing.T) {
	r := newNotifyRig(t)
	resp, data := r.do(t, "GET", "/api/v1/notifications", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get = %d %s", resp.StatusCode, data)
	}
	var view map[string]any
	json.Unmarshal(data, &view)
	for _, k := range []string{"smtp_enabled", "webhook_enabled", "teams_enabled", "smtp_password_set"} {
		if view[k] != false {
			t.Errorf("%s = %v on a fresh install", k, view[k])
		}
	}
	if _, leaked := view["smtp_password"]; leaked {
		t.Fatal("the password must never appear in a response")
	}
}

func TestNotificationsRoundTripHidesThePassword(t *testing.T) {
	r := newNotifyRig(t)
	body := map[string]any{"public_url": "https://hub.example", "smtp_enabled": true,
		"smtp_host": "smtp.example", "smtp_port": 587, "smtp_username": "u",
		"smtp_password": "secret", "smtp_from": "hub@example",
		"smtp_to": []string{"a@example"}, "smtp_tls": "starttls"}
	if resp, data := r.do(t, "PUT", "/api/v1/notifications", body); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	if n := r.notifier(t).reloads; n != 1 {
		t.Fatalf("saving settings must reload the channels, reloads = %d", n)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications", nil)
	var view map[string]any
	json.Unmarshal(data, &view)
	if view["smtp_password_set"] != true {
		t.Fatal("smtp_password_set must say a password exists")
	}
	if _, leaked := view["smtp_password"]; leaked {
		t.Fatal("the password leaked into the response")
	}
	if view["smtp_username"] != "u" || view["smtp_host"] != "smtp.example" {
		t.Fatalf("view = %v", view)
	}
}

func TestAbsentPasswordKeepsTheStoredOne(t *testing.T) {
	// The browser never receives the password, so it cannot send it back. A save
	// that omits the field must not silently erase it.
	r := newNotifyRig(t)
	base := map[string]any{"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
		"smtp_from": "f@example", "smtp_to": []string{"a@example"}, "smtp_tls": "none"}
	with := map[string]any{}
	for k, v := range base {
		with[k] = v
	}
	with["smtp_password"] = "secret"
	r.do(t, "PUT", "/api/v1/notifications", with)

	base["smtp_host"] = "h2"
	if resp, data := r.do(t, "PUT", "/api/v1/notifications", base); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	got, _, _ := r.st.GetSetting("notify_smtp_password")
	if got != "secret" {
		t.Fatalf("password = %q, want the stored one kept", got)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications", nil)
	var view map[string]any
	json.Unmarshal(data, &view)
	if view["smtp_host"] != "h2" {
		t.Fatalf("the rest of the save must still apply, host = %v", view["smtp_host"])
	}
}

func TestEmptyPasswordClearsIt(t *testing.T) {
	r := newNotifyRig(t)
	body := map[string]any{"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
		"smtp_from": "f@example", "smtp_to": []string{"a@example"}, "smtp_tls": "none",
		"smtp_password": "secret"}
	r.do(t, "PUT", "/api/v1/notifications", body)
	body["smtp_password"] = ""
	r.do(t, "PUT", "/api/v1/notifications", body)
	if got, _, _ := r.st.GetSetting("notify_smtp_password"); got != "" {
		t.Fatalf("an explicit empty string must clear the password, got %q", got)
	}
}

func TestInvalidConfigIsRejectedWithItsReason(t *testing.T) {
	r := newNotifyRig(t)
	cases := map[string]map[string]any{
		"smtp with no recipient": {"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
			"smtp_from": "f", "smtp_to": []string{}, "smtp_tls": "none"},
		"teams over http":      {"teams_enabled": true, "teams_url": "http://example/x"},
		"public url not a url": {"public_url": "hub.example"},
	}
	for name, body := range cases {
		resp, data := r.do(t, "PUT", "/api/v1/notifications", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%s)", name, resp.StatusCode, data)
		}
	}
	if n := r.notifier(t).reloads; n != 0 {
		t.Fatalf("a rejected save must not reload anything, reloads = %d", n)
	}
	if v, _, _ := r.st.GetSetting("notify_teams_url"); v != "" {
		t.Fatalf("a rejected save must write nothing, teams url = %q", v)
	}
}

func TestTestEndpointReportsTheRealFailure(t *testing.T) {
	r := newNotifyRig(t)
	r.notifier(t).err = errors.New("Bad Gateway: workflow not found")
	resp, data := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "teams"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(data), "workflow not found") {
		t.Fatalf("the reason is the whole point of a test button: %s", data)
	}
	if tested := r.notifier(t).tested; len(tested) != 1 || tested[0] != "teams" {
		t.Fatalf("tested = %v", tested)
	}
}

func TestTestEndpointSucceeds(t *testing.T) {
	r := newNotifyRig(t)
	resp, data := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "smtp"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("code = %d %s", resp.StatusCode, data)
	}
	if tested := r.notifier(t).tested; tested[0] != "smtp" {
		t.Fatalf("tested = %v", tested)
	}
}

func TestTestEndpointRejectsAnUnknownChannel(t *testing.T) {
	r := newNotifyRig(t)
	resp, _ := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "carrier-pigeon"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", resp.StatusCode)
	}
	if tested := r.notifier(t).tested; len(tested) != 0 {
		t.Fatalf("an unknown channel must not reach the hub, tested = %v", tested)
	}
}

func TestDeliveriesListShowsTheOutcome(t *testing.T) {
	r := newNotifyRig(t)
	hostID, _ := r.newHost(t, "pi")
	ruleID := r.newRule(t, nil, "cpu")
	e, err := r.st.InsertAlertEvent(store.AlertEvent{RuleID: ruleID, HostID: hostID,
		Metric: "cpu", Kind: "fired", Value: 99, At: r.now})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.st.CreateDelivery(e.ID, "webhook", r.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.MarkDeliveryFailed(d.ID, r.now, 4, "503 Service Unavailable"); err != nil {
		t.Fatal(err)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications/deliveries", nil)
	var out []map[string]any
	json.Unmarshal(data, &out)
	if len(out) != 1 {
		t.Fatalf("deliveries = %s", data)
	}
	if out[0]["state"] != "failed" || out[0]["channel"] != "webhook" ||
		out[0]["last_error"] != "503 Service Unavailable" || out[0]["host"] != "pi" ||
		out[0]["kind"] != "fired" {
		t.Fatalf("delivery view = %v", out[0])
	}
}

func TestNotificationsRequireAuth(t *testing.T) {
	r := newRig(t) // deliberately not logged in
	for _, path := range []string{"/api/v1/notifications", "/api/v1/notifications/deliveries"} {
		if resp, _ := r.do(t, "GET", path, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", path, resp.StatusCode)
		}
	}
}
```

Two changes to `internal/hub/server/server_test.go` make those tests compile,
and leave every existing test untouched:

```go
type rig struct {
	st     *store.Store
	srv    *httptest.Server
	client *http.Client
	bus    *Broadcaster
	agents *fakeAgents
	notify Notifier          // new
	now    time.Time
}
```
and in `newRig`, build one and pass it in `Deps`:
```go
	r := &rig{st: st, bus: NewBroadcaster(), agents: &fakeAgents{}, notify: &fakeNotifier{},
		now: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)}
	h := New(Deps{Store: st, Agents: r.agents, Bus: r.bus, Notify: r.notify, Static: fs.FS(static),
		InstallScript: []byte("#!/bin/sh\necho hi\n"),
		Now: func() time.Time { return r.now }, Version: "test", Log: slog.Default(), SessionTTL: time.Hour})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/server/ -run Notif -v`
Expected: FAIL, the routes do not exist.

- [ ] **Step 3: Write api_notify.go**

```go
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// Notifier is the hub seen from the server: reload the channels after a save,
// and deliver a test message on demand.
type Notifier interface {
	ReloadNotify()
	TestNotify(ctx context.Context, channel string) error
}

// notifyView is what the browser sees. The password is represented by a bool:
// it goes in, it never comes back.
type notifyView struct {
	PublicURL string `json:"public_url"`

	SMTPEnabled     bool     `json:"smtp_enabled"`
	SMTPHost        string   `json:"smtp_host"`
	SMTPPort        int      `json:"smtp_port"`
	SMTPUsername    string   `json:"smtp_username"`
	SMTPPasswordSet bool     `json:"smtp_password_set"`
	SMTPFrom        string   `json:"smtp_from"`
	SMTPTo          []string `json:"smtp_to"`
	SMTPTLS         string   `json:"smtp_tls"`

	WebhookEnabled bool              `json:"webhook_enabled"`
	WebhookURL     string            `json:"webhook_url"`
	WebhookHeaders map[string]string `json:"webhook_headers"`

	TeamsEnabled bool   `json:"teams_enabled"`
	TeamsURL     string `json:"teams_url"`

	Health map[string]healthView `json:"health"`
}

type healthView struct {
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	At        time.Time `json:"at"`
}

// notifyInput mirrors notifyView for writes. Password is a pointer so that
// "absent" and "empty" stay different answers.
type notifyInput struct {
	PublicURL string `json:"public_url"`

	SMTPEnabled  bool     `json:"smtp_enabled"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPUsername string   `json:"smtp_username"`
	SMTPPassword *string  `json:"smtp_password"`
	SMTPFrom     string   `json:"smtp_from"`
	SMTPTo       []string `json:"smtp_to"`
	SMTPTLS      string   `json:"smtp_tls"`

	WebhookEnabled bool              `json:"webhook_enabled"`
	WebhookURL     string            `json:"webhook_url"`
	WebhookHeaders map[string]string `json:"webhook_headers"`

	TeamsEnabled bool   `json:"teams_enabled"`
	TeamsURL     string `json:"teams_url"`
}

func (s *server) handleGetNotifications(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := notify.LoadConfig(s.Store)
	health := map[string]healthView{}
	if hs, err := s.Store.ChannelHealth(); err == nil {
		for name, d := range hs {
			health[name] = healthView{State: d.State, LastError: d.LastError, At: d.UpdatedAt}
		}
	}
	if c.Webhook.Headers == nil {
		c.Webhook.Headers = map[string]string{}
	}
	writeJSON(w, http.StatusOK, notifyView{
		PublicURL:       c.Public,
		SMTPEnabled:     c.SMTP.Enabled,
		SMTPHost:        c.SMTP.Host,
		SMTPPort:        c.SMTP.Port,
		SMTPUsername:    c.SMTP.Username,
		SMTPPasswordSet: c.SMTP.Password != "",
		SMTPFrom:        c.SMTP.From,
		SMTPTo:          c.SMTP.To,
		SMTPTLS:         c.SMTP.TLSMode,
		WebhookEnabled:  c.Webhook.Enabled,
		WebhookURL:      c.Webhook.URL,
		WebhookHeaders:  c.Webhook.Headers,
		TeamsEnabled:    c.Teams.Enabled,
		TeamsURL:        c.Teams.URL,
		Health:          health,
	})
}

func (s *server) handlePutNotifications(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in notifyInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	current := notify.LoadConfig(s.Store)
	password := current.SMTP.Password
	if in.SMTPPassword != nil { // present in the JSON: the user meant it
		password = *in.SMTPPassword
	}
	if in.WebhookHeaders == nil {
		in.WebhookHeaders = map[string]string{}
	}
	if in.SMTPTLS == "" {
		in.SMTPTLS = "starttls"
	}
	if in.SMTPPort == 0 {
		in.SMTPPort = 587
	}
	c := notify.Config{
		Public: in.PublicURL,
		SMTP: notify.SMTPConfig{Enabled: in.SMTPEnabled, Host: in.SMTPHost, Port: in.SMTPPort,
			Username: in.SMTPUsername, Password: password, From: in.SMTPFrom, To: in.SMTPTo, TLSMode: in.SMTPTLS},
		Webhook: notify.WebhookConfig{Enabled: in.WebhookEnabled, URL: in.WebhookURL, Headers: in.WebhookHeaders},
		Teams:   notify.TeamsConfig{Enabled: in.TeamsEnabled, URL: in.TeamsURL},
	}
	// Validate before writing: a partial save would leave the hub in a state the
	// user never asked for.
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := notify.SaveConfig(s.Store, c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Notify.ReloadNotify()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleTestNotification(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body struct {
		Channel string `json:"channel"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	switch body.Channel {
	case "smtp", "webhook", "teams":
	default:
		writeErr(w, http.StatusBadRequest, "unknown channel")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Notify.TestNotify(ctx, body.Channel); err != nil {
		// 502, with the channel's own words: a test button that says only
		// "failed" is a test button nobody can act on.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deliveryView struct {
	ID        int64     `json:"id"`
	Channel   string    `json:"channel"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	Host      string    `json:"host"`
	Metric    string    `json:"metric"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
}

func (s *server) handleDeliveries(w http.ResponseWriter, r *http.Request, _ store.User) {
	ds, err := s.Store.ListDeliveries(100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]deliveryView, 0, len(ds))
	for _, d := range ds {
		v := deliveryView{ID: d.ID, Channel: d.Channel, State: d.State, Attempts: d.Attempts,
			LastError: d.LastError, At: d.UpdatedAt}
		if e, host, err := s.Store.DeliveryEvent(d.ID); err == nil {
			v.Host, v.Metric, v.Kind = host.Name, e.Metric, e.Kind
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}
```

In `server.go`, add `Notify Notifier` to `Deps`, carry it onto `server`, and
register the routes next to the settings block:
```go
	mux.Handle("GET /api/v1/notifications", s.auth(s.handleGetNotifications))
	mux.Handle("PUT /api/v1/notifications", s.auth(s.handlePutNotifications))
	mux.Handle("POST /api/v1/notifications/test", s.auth(s.handleTestNotification))
	mux.Handle("GET /api/v1/notifications/deliveries", s.auth(s.handleDeliveries))
```

- [ ] **Step 4: Implement `TestNotify` on the hub**

In `internal/hub/hub.go`, satisfying `server.Notifier`. `ReloadNotify` already
carries the exported name from Task 7, so only `TestNotify` is new here.

```go
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
			Threshold: 90, Duration: 10 * time.Minute, At: time.Now().UTC(), Link: cfg.Link(0),
		})
	}
	return fmt.Errorf("channel %s is not enabled", name)
}
```

Pass the hub into the server in `New`: `server.Deps{… Notify: h …}`. The `Hub`
value must exist before `server.New` is called, which it already does.

- [ ] **Step 5: Run everything**

Run: `go test ./... -race` then `go vet ./...`
Expected: PASS. A test elsewhere that builds `server.Deps` without `Notify` will
panic on nil; give those a small stub rather than a nil check in the handler.

- [ ] **Step 6: Commit**

```bash
git add internal/hub
git commit -m "feat(api): notifications settings, test button and delivery log"
```

---

### Task 9: The notifications tab

**Files:**
- Modify: `web/src/lib/api.ts`, `web/src/routes/settings/+page.svelte`
- Test: `web/src/lib/notify.test.ts`, `web/src/lib/notify.ts`

**Interfaces:**
- Produces:
```ts
export interface NotifyHealth { state: string; last_error?: string; at: string }
export interface Notifications { public_url: string; smtp_enabled: boolean; smtp_host: string;
  smtp_port: number; smtp_username: string; smtp_password_set: boolean; smtp_from: string;
  smtp_to: string[]; smtp_tls: string; webhook_enabled: boolean; webhook_url: string;
  webhook_headers: Record<string, string>; teams_enabled: boolean; teams_url: string;
  health: Record<string, NotifyHealth> }
export interface Delivery { id: number; channel: string; state: string; attempts: number;
  last_error?: string; host: string; metric: string; kind: string; at: string }
// web/src/lib/notify.ts
export function toPayload(v: Notifications, password: string | null): Record<string, unknown>
export function parseRecipients(raw: string): string[]
export function healthLabel(h: NotifyHealth | undefined): string
```

- [ ] **Step 1: Write the failing front-end tests**

`web/src/lib/notify.test.ts`:
```ts
import { describe, expect, it } from 'vitest';
import { healthLabel, parseRecipients, toPayload } from './notify';
import type { Notifications } from './api';

const base: Notifications = {
  public_url: 'https://hub.example',
  smtp_enabled: true, smtp_host: 'smtp.example', smtp_port: 587, smtp_username: 'u',
  smtp_password_set: true, smtp_from: 'hub@example', smtp_to: ['a@example'], smtp_tls: 'starttls',
  webhook_enabled: false, webhook_url: '', webhook_headers: {},
  teams_enabled: false, teams_url: '', health: {}
};

describe('toPayload', () => {
  it('omits the password when the field was not touched', () => {
    const p = toPayload(base, null);
    expect('smtp_password' in p).toBe(false);
  });

  it('sends the password when the user typed one', () => {
    expect(toPayload(base, 'hunter2').smtp_password).toBe('hunter2');
  });

  it('sends an empty string when the user cleared it', () => {
    expect(toPayload(base, '').smtp_password).toBe('');
  });

  it('never sends the read-only flag back', () => {
    expect('smtp_password_set' in toPayload(base, null)).toBe(false);
    expect('health' in toPayload(base, null)).toBe(false);
  });
});

describe('parseRecipients', () => {
  it('splits on commas and newlines and drops blanks', () => {
    expect(parseRecipients(' a@x ,\n b@x ,,\n')).toEqual(['a@x', 'b@x']);
  });
  it('returns an empty list for empty input', () => {
    expect(parseRecipients('   ')).toEqual([]);
  });
});

describe('healthLabel', () => {
  it('says nothing has been sent yet when there is no history', () => {
    expect(healthLabel(undefined)).toBe('never used');
  });
  it('reports the failure reason', () => {
    expect(healthLabel({ state: 'failed', last_error: '503', at: '2026-09-18T03:00:00Z' })).toContain('503');
  });
  it('reports success plainly', () => {
    expect(healthLabel({ state: 'sent', at: '2026-09-18T03:00:00Z' })).toBe('last delivery succeeded');
  });
});
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && npx vitest run src/lib/notify.test.ts`
Expected: FAIL, the module does not exist.

- [ ] **Step 3: Write `web/src/lib/notify.ts`**

```ts
import type { Notifications, NotifyHealth } from './api';

/** Splits a textarea of recipients on commas or newlines, dropping blanks. */
export function parseRecipients(raw: string): string[] {
  return raw
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/**
 * Builds the PUT body. The password is the only field the server sends back as
 * a boolean, so it is the only one carried separately: null means "leave the
 * stored one alone", a string means "use this", including the empty string,
 * which clears it.
 */
export function toPayload(v: Notifications, password: string | null): Record<string, unknown> {
  const { smtp_password_set: _set, health: _health, ...rest } = v;
  const out: Record<string, unknown> = { ...rest };
  if (password !== null) out.smtp_password = password;
  return out;
}

export function healthLabel(h: NotifyHealth | undefined): string {
  if (!h) return 'never used';
  if (h.state === 'sent') return 'last delivery succeeded';
  if (h.state === 'pending') return 'delivery in flight';
  return `last delivery failed: ${h.last_error ?? 'no reason given'}`;
}
```

- [ ] **Step 4: Run the tests**

Run: `cd web && npx vitest run`
Expected: PASS, the 12 lot 1 tests plus these 9.

- [ ] **Step 5: Add the types to `api.ts` and the tab to the settings page**

Append the three interfaces above to `web/src/lib/api.ts`.

In `web/src/routes/settings/+page.svelte`, extend the tab union and add the
panel. The page already uses `$state`, `api.get`, `api.put`, `api.post`, `msg`
and `err`; follow those, do not introduce a second pattern.

```svelte
  type Tab = 'hosts' | 'alerts' | 'notifications' | 'system';
```

State:
```svelte
  let notif = $state<Notifications | null>(null);
  let smtpPassword = $state<string | null>(null); // null = untouched
  let recipients = $state('');
  let headersText = $state('');
  let deliveries = $state<Delivery[]>([]);
  let testing = $state('');
```

Loading, inside the existing `load()`:
```svelte
    const n = await api.get<Notifications>('/api/v1/notifications');
    notif = n;
    recipients = n.smtp_to.join('\n');
    headersText = Object.entries(n.webhook_headers).map(([k, v]) => `${k}: ${v}`).join('\n');
    deliveries = await api.get<Delivery[]>('/api/v1/notifications/deliveries');
```

Saving:
```svelte
  function parseHeaders(raw: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const line of raw.split('\n')) {
      const i = line.indexOf(':');
      if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
    return out;
  }

  async function saveNotifications() {
    if (!notif) return;
    err = '';
    msg = '';
    try {
      const payload = toPayload({ ...notif, smtp_to: parseRecipients(recipients),
        webhook_headers: parseHeaders(headersText) }, smtpPassword);
      await api.put('/api/v1/notifications', payload);
      smtpPassword = null;
      msg = 'Notifications saved';
      await load();
    } catch (e) {
      err = String(e);
    }
  }

  async function testChannel(channel: string) {
    testing = channel;
    err = '';
    msg = '';
    try {
      await api.post('/api/v1/notifications/test', { channel });
      msg = `Test message sent through ${channel}`;
    } catch (e) {
      err = String(e); // the server puts the channel's own words in the body
    } finally {
      testing = '';
      deliveries = await api.get<Delivery[]>('/api/v1/notifications/deliveries');
    }
  }
```

The panel, following the markup of the existing panels: one `<fieldset>` per
channel with its enable checkbox, its fields, a **Send test** button disabled
while `testing` is set or while the channel is off, and `healthLabel` under the
title. Below the three, a delivery table with columns Channel, Host, Event,
State, Attempts, Reason, When, rendering `—` for an empty reason. The password
input binds through a handler rather than directly, because `null` and `''` must
stay distinct:
```svelte
  <input type="password" autocomplete="new-password"
    placeholder={notif.smtp_password_set ? '•••••••• (unchanged)' : 'no password set'}
    value={smtpPassword ?? ''}
    oninput={(e) => (smtpPassword = (e.currentTarget as HTMLInputElement).value)} />
  {#if smtpPassword === ''}<p class="hint">Saving now clears the stored password.</p>{/if}
```

Teams gets one line of help text under its URL field, because the failure it
prevents is silent:

> Paste the HTTP URL of a Power Automate workflow using the "When a Teams
> webhook request is received" trigger. Office 365 connector URLs
> (`outlook.office.com/webhook/…`) stopped working in May 2026.

- [ ] **Step 6: Build and check**

Run: `cd web && npx svelte-check --threshold error && npm run build`
Expected: zero errors, and `web/build/` regenerated.
Then `go build ./...` so the embedded assets compile.

- [ ] **Step 7: Commit**

```bash
git add web internal
git commit -m "feat(web): notifications tab with per-channel test and delivery log"
```

---

### Task 10: End-to-end proof, docs and the branch

**Files:**
- Modify: `internal/e2e/e2e_test.go`, `README.md`, `docs/ARCHITECTURE.md`, `docs/ARCHITECTURE_EN.md`
- Test: the whole suite

- [ ] **Step 1: Extend the end-to-end test**

Add to `internal/e2e/e2e_test.go` a case that runs the real hub with a real
agent and a catching webhook, and asserts a delivery is recorded as sent. Follow
the existing test's setup; do not build a second harness.

```go
func TestAlertReachesAWebhookEndToEnd(t *testing.T) {
	// The one test that exercises agent → store → evaluator → dispatcher →
	// channel. Everything else stubs at least one of those seams.
	hits := make(chan string, 8)
	catcher := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		hits <- string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer catcher.Close()
	// … start the hub as the existing e2e test does, then:
	//   1. save a notify config enabling the webhook at catcher.URL
	//   2. create a host and connect an agent
	//   3. create a rule with threshold 0 so the next sample fires it
	//   4. wait on hits for the payload, assert host and kind
	//   5. poll /api/v1/notifications/deliveries until one row reads "sent"
}
```

Write the body against the harness that is actually in the file. Assert on the
payload's `host` and `kind`, and on a delivery row in state `sent`.

- [ ] **Step 2: Run the full suite**

```bash
go test ./... -race
go vet ./...
cd web && npx vitest run && npx svelte-check --threshold error && npm run build
```
Expected: everything passes, including the 110 lot 1 Go tests.

- [ ] **Step 3: Verify against a running hub, by hand**

Tests did not catch six of lot 1's defects. Repeat the discipline:

1. Build and start: `go build -o /tmp/smhub ./cmd/smhub && SM_DATA_DIR=/tmp/smdata /tmp/smhub &`
2. Run a local mail catcher: `python3 -m smtpd -n -c DebuggingServer localhost:1025`
   is gone from Python 3.12. Use `docker run --rm -p 1025:1025 -p 8025:8025 mailhog/mailhog`
   and read the caught mail at `http://localhost:8025`. If Docker is not
   available, run `nc -l 1025` and read the raw conversation.
3. In the browser: configure SMTP against that catcher, press **Send test**,
   confirm the message arrives and renders.
4. Configure a webhook against `nc -l 9999`, press **Send test**, confirm the
   JSON body.
5. Force a real alert: create a CPU rule with threshold 0 and duration 0,
   wait one evaluation tick, confirm both channels deliver and the delivery
   table shows two `sent` rows.
6. Break it on purpose: point the webhook at `http://127.0.0.1:1` and fire
   another alert. Confirm the row reaches `failed` after roughly 31 seconds,
   with a reason, and that the hub kept serving throughout.
7. Restart mid-flight: fire an alert against a webhook that hangs, kill the hub,
   restart it, confirm the pending delivery is replayed.
8. Check the console for errors on every page.

Record what each step actually produced. A step that was not run is reported as
not run.

- [ ] **Step 4: Teams, which only Vincent can confirm**

Teams was verified against the payload shape and the HTTP contract, never
against a tenant. Ask Vincent to paste a workflow URL, press **Send test**, and
say what Teams showed. Until he does, the README says Teams is untested against
a live tenant. Do not claim otherwise on the strength of a green test.

- [ ] **Step 5: Documentation**

- `README.md`: a Notifications section covering the three channels, the
  `public_url` setting and why links are absent without it, the Teams workflow
  caveat, and one line saying delivery is at least once, so a hub that crashes
  between the send and the record repeats a message on restart. Tick lot 2 in
  the roadmap.
- `docs/ARCHITECTURE_EN.md`: the `notify` package, the `deliveries` table, the
  retry schedule, the drop policy. `docs/ARCHITECTURE.md` mirrors it in French
  in the same turn.
- `CHANGES.md`, `TODOS.md`, `MEMORY.md`: updated **in the same turn as each
  task**, per the repository's own maintenance rule, not collected here. This
  step is the final reconciliation, not the first entry. They are gitignored.

- [ ] **Step 6: Finish the branch**

**REQUIRED SUB-SKILL:** use `superpowers:finishing-a-development-branch`.
Push `feat/lot2-channels`, open the pull request, wait for CI on `go`, `web` and
`docker`, then merge into `main`. Never push to `main` directly.

```bash
git push -u origin feat/lot2-channels
gh pr create --title "feat: alert channels (lot 2)" --body "…"
gh pr checks --watch
```

---

## Self-review

**Spec coverage.** §2 hooks → Task 7 (`evaluate` calls `notify`). §3 rendering →
Task 2. §4 queue, retries, drop policy → Task 6. §5 `deliveries` table → Task 1.
§6 the three channels and the verified Teams payload → Tasks 3, 4, 5. §7 settings
and the password rule → Tasks 7 and 8. §8 the interface → Task 9. Verification →
Task 10.

**Ordering.** `channel.go`'s `Enabled` references all three constructors, so it
compiles only from Task 5 onward. Task 2 says so and tells the executor how to
proceed. Nothing else is forward-referenced.

**Names checked across tasks.** `MarkDeliverySent`/`MarkDeliveryFailed` carry the
same signature in Task 1, the `Recorder` interface in Task 6, and the recorder
stub in its tests. `Message` fields are identical in Tasks 2, 3, 4, 5 and 7.
`classifyHTTP(status int, detail []byte)` is defined once in Task 4 and reused in
Task 5. `ReloadNotify` is exported from Task 7 onward, which is the name the
`Notifier` interface of Task 8 requires.

**Every task boundary builds.** `Enabled` lives in Task 5, alongside the three
constructors it calls, so no commit in this plan leaves `go build ./...` red.

**What this plan does not prove.** Teams against a real tenant, and SMTP against
a relay that enforces SPF or DKIM. Both are named where they matter rather than
hidden behind a passing test.
