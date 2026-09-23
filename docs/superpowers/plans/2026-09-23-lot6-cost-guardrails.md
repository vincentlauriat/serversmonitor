# ServersMonitor Lot 6 (cost guardrails) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Budget alerts (thresholds, end-of-month projection, per-resource share), orphan detection with delete-on-request, and weekly stop schedules — all delivered through the alert channels, action log and inventory that lots 2–5 already have.

**Architecture:** A new stateless package `internal/hub/guardrails` computes wanted states from inventory, costs, settings and a clock, and returns only the transitions to journal. The hub calls it after each successful sync (budget, orphans) and on its minute tick (schedules). Transitions land in a new append-only table `azure_guardrail_events`; the lot 2 dispatcher delivers them through a rebuilt `deliveries` table that can point at either event table. Schedules issue lot 4 actions marked `origin=schedule`. Orphans are two columns on `azure_resources`, recomputed each sweep from typed reads in the `azure` package.

**Tech Stack:** Go standard library only (plus the existing `modernc.org/sqlite` driver and `time/tzdata`), SvelteKit front, vitest. No Azure SDK, as in lots 3–5.

**Spec:** `docs/superpowers/specs/2026-09-23-lot6-cost-guardrails-design.md`

## Global Constraints

- Branch `feat/lot6-cost-guardrails` (already holds the spec); never commit to `main`. Conventional commits, English, no `Co-Authored-By` trailer.
- Go 1.26, `CGO_ENABLED=0`, `go test ./... -race` green before every commit; `cd web && npm run check && npm test` green before every front commit.
- Every task ends with a mutation check: break the code the test protects, run the test, see it fail, restore, run again.
- "Silence is not zero": nothing is evaluated after a failed sync; a typed read that answers 403 yields `unverified`, never healthy.
- Nothing is deleted without a typed name. Automatic deletion does not exist.
- One time zone for the hub, setting `azure_timezone`, default `Europe/Paris`; `time/tzdata` is imported so the binary carries the zone database (`FROM scratch` has none).
- Resource ids are lowercased with `azure.NormalizeID` on the way in; ARM ids with original casing (`ARMID`) are what requests are built from.
- Verbs: a VM's `stop` is already `deallocate` in the `azure.providers` table (lot 5). The spec's "gains `deallocate`" is satisfied; the page merely labels the button *Deallocate* for VMs. Do not add a fourth `Action` value.

---

## File map

| File | Responsibility |
|---|---|
| `internal/hub/store/migrations/0006_guardrails.sql` | schema: `azure_schedules`, `azure_guardrail_events`, orphan columns, `origin` on actions, `deliveries` rebuilt with two nullable event references |
| `internal/hub/store/guardrails.go` (+`_test.go`) | guardrail events: insert, last-per-instance, list; guardrail deliveries |
| `internal/hub/store/azure_schedules.go` (+`_test.go`) | schedules CRUD and boundary bookkeeping |
| `internal/hub/store/azure.go`, `azure_actions.go`, `deliveries.go` | orphan columns, `Origin` on actions, `GuardrailEventID` on deliveries |
| `internal/hub/guardrails/settings.go` (+`_test.go`) | the four settings: load, save, validate |
| `internal/hub/guardrails/budget.go` (+`_test.go`) | pure budget evaluator |
| `internal/hub/guardrails/orphans.go` (+`_test.go`) | pure orphan evaluator (reasons → transitions, `orphan_since`) |
| `internal/hub/guardrails/schedule.go` (+`_test.go`) | windows: parse, merge, wanted state, last boundary, DST |
| `internal/hub/azure/orphans.go` (+`_test.go`) | typed reads that decide each orphan reason; 403 → unverified, 404 → skip |
| `internal/hub/azure/delete.go` | `apiVersionFor` learns `disk`, `plan`; `DeleteAny` without the ours-check |
| `internal/hub/guardrails_hub.go` (+`_test.go`) | hub wiring: evaluate after syncs, minute tick for schedules, catch-up, deliveries, SSE |
| `internal/hub/notify.go` | guardrail messages and replay of guardrail deliveries |
| `internal/hub/server/api_guardrails.go` (+`_test.go`) | routes of spec §4 |
| `web/src/lib/api.ts`, `web/src/lib/guardrails.ts` (+`.test.ts`) | types and pure helpers for the page |
| `web/src/routes/azure/+page.svelte`, `web/src/routes/settings/+page.svelte` | the three blocks and the settings block |
| `README.md`, `docs/ARCHITECTURE_EN.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` | docs |

---

### Task 1: Migration 6 and guardrail events in the store

**Files:**
- Create: `internal/hub/store/migrations/0006_guardrails.sql`
- Create: `internal/hub/store/guardrails.go`
- Create: `internal/hub/store/guardrails_test.go`
- Modify: `internal/hub/store/deliveries.go` (struct, columns, `CreateGuardrailDelivery`, `DeliveryGuardrailEvent`)

**Interfaces:**
- Produces:
  ```go
  type GuardrailEvent struct {
      ID      int64
      Subject string  // "budget" or a lowercased ARM id
      Rule    string  // budget_threshold | budget_projection | resource_share | orphan | schedule_failed
      Detail  string  // "80", "100" for thresholds; "" otherwise
      Kind    string  // fired | resolved
      Value   float64
      At      time.Time
  }
  type GuardrailKey struct{ Subject, Rule, Detail string }
  func (s *Store) InsertGuardrailEvent(e GuardrailEvent) (GuardrailEvent, error)
  func (s *Store) LastGuardrailEventPerKey() (map[GuardrailKey]GuardrailEvent, error)
  func (s *Store) ListGuardrailEvents(limit int) ([]GuardrailEvent, error)
  func (s *Store) CreateGuardrailDelivery(eventID int64, channel string, now time.Time) (Delivery, error)
  func (s *Store) DeliveryGuardrailEvent(deliveryID int64) (GuardrailEvent, error)
  // Delivery gains: GuardrailEventID *int64 ; EventID becomes *int64
  ```

- [ ] **Step 1: Write the migration**

```sql
-- 0006_guardrails.sql
-- Lot 6. Guardrail alerts have no host, so they get their own append-only
-- journal rather than a fake host in alert_events. Deliveries must be able to
-- point at either journal, and SQLite cannot relax a NOT NULL column in
-- place, so the table is rebuilt.

CREATE TABLE azure_guardrail_events (
  id      INTEGER PRIMARY KEY,
  subject TEXT NOT NULL,        -- 'budget' | lowercased ARM id
  rule    TEXT NOT NULL,        -- budget_threshold | budget_projection | resource_share | orphan | schedule_failed
  detail  TEXT NOT NULL DEFAULT '',
  kind    TEXT NOT NULL,        -- fired | resolved
  value   REAL NOT NULL,
  at      TEXT NOT NULL
);
CREATE INDEX azure_guardrail_events_key ON azure_guardrail_events(subject, rule, detail, id);

CREATE TABLE azure_schedules (
  resource_id     TEXT PRIMARY KEY,   -- lowercased, as azure_resources.id
  off_windows     TEXT NOT NULL,      -- JSON: [{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]
  enabled         INTEGER NOT NULL DEFAULT 1,
  last_boundary   TEXT,               -- RFC 3339 UTC of the last boundary acted on
  last_applied_at TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
) WITHOUT ROWID;

ALTER TABLE azure_resources ADD COLUMN orphan_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE azure_resources ADD COLUMN orphan_since  TEXT;
ALTER TABLE azure_actions   ADD COLUMN origin        TEXT NOT NULL DEFAULT 'user';

CREATE TABLE deliveries_new (
  id                 INTEGER PRIMARY KEY,
  event_id           INTEGER REFERENCES alert_events(id) ON DELETE CASCADE,
  guardrail_event_id INTEGER REFERENCES azure_guardrail_events(id) ON DELETE CASCADE,
  channel            TEXT NOT NULL,
  state              TEXT NOT NULL,
  attempts           INTEGER NOT NULL DEFAULT 0,
  last_error         TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  CHECK ((event_id IS NULL) <> (guardrail_event_id IS NULL))
);
INSERT INTO deliveries_new (id, event_id, channel, state, attempts, last_error, created_at, updated_at)
  SELECT id, event_id, channel, state, attempts, last_error, created_at, updated_at FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_new RENAME TO deliveries;
CREATE INDEX deliveries_state   ON deliveries(state, id);
CREATE INDEX deliveries_channel ON deliveries(channel, id);
```

- [ ] **Step 2: Write the failing tests**

```go
// internal/hub/store/guardrails_test.go
package store

import (
	"testing"
	"time"
)

func TestGuardrailEventsJournalOnlyWhatIsInserted(t *testing.T) {
	s := openTest(t) // the helper every store test uses; see alerts_test.go
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	e, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "fired", Value: 41.2, At: now})
	if err != nil || e.ID == 0 {
		t.Fatalf("insert: %v id=%d", err, e.ID)
	}
	if _, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "resolved", Value: 0, At: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertGuardrailEvent(GuardrailEvent{Subject: "/subscriptions/x/disk1", Rule: "orphan", Kind: "fired", Value: 3, At: now}); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastGuardrailEventPerKey()
	if err != nil {
		t.Fatal(err)
	}
	if got := last[GuardrailKey{"budget", "budget_threshold", "80"}]; got.Kind != "resolved" {
		t.Fatalf("last for the 80 threshold = %q, want resolved", got.Kind)
	}
	if got := last[GuardrailKey{"/subscriptions/x/disk1", "orphan", ""}]; got.Kind != "fired" || got.Value != 3 {
		t.Fatalf("last orphan = %+v", got)
	}
	list, err := s.ListGuardrailEvents(2)
	if err != nil || len(list) != 2 || list[0].Kind != "resolved" && list[0].Rule != "orphan" {
		t.Fatalf("list newest first, 2 rows: %v %+v", err, list)
	}
}

func TestADeliveryPointsAtExactlyOneJournal(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	e, _ := s.InsertGuardrailEvent(GuardrailEvent{Subject: "budget", Rule: "budget_projection", Kind: "fired", Value: 120, At: now})
	d, err := s.CreateGuardrailDelivery(e.ID, "smtp", now)
	if err != nil {
		t.Fatal(err)
	}
	if d.EventID != nil || d.GuardrailEventID == nil || *d.GuardrailEventID != e.ID {
		t.Fatalf("delivery = %+v", d)
	}
	got, err := s.DeliveryGuardrailEvent(d.ID)
	if err != nil || got.Rule != "budget_projection" {
		t.Fatalf("read back: %v %+v", err, got)
	}
	// An alert delivery still works the old way, and answers ErrNotFound on
	// the guardrail accessor rather than a zero event.
	h := seedHost(t, s) // helper from hosts_test.go; adjust to the existing one
	ae, _ := s.InsertAlertEvent(AlertEvent{RuleID: 0, HostID: h.ID, Metric: "status", Kind: "fired", Value: 1, At: now})
	ad, err := s.CreateDelivery(ae.ID, "smtp", now)
	if err != nil || ad.EventID == nil || *ad.EventID != ae.ID || ad.GuardrailEventID != nil {
		t.Fatalf("alert delivery = %+v %v", ad, err)
	}
	if _, err := s.DeliveryGuardrailEvent(ad.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound for an alert delivery, got %v", err)
	}
	// Pending deliveries of both kinds come back together, in id order.
	pend, _ := s.PendingDeliveries()
	if len(pend) != 2 || pend[0].ID != d.ID {
		t.Fatalf("pending = %+v", pend)
	}
}
```

Read `internal/hub/store/alerts_test.go` and `hosts_test.go` first for the exact names of the open and seed helpers, and use those names.

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/hub/store/ -run 'TestGuardrailEvents|TestADeliveryPointsAt' -v`
Expected: FAIL — undefined `GuardrailEvent`, `CreateGuardrailDelivery`; then, after the migration alone, a compile error on `d.EventID != nil` because `EventID` is still `int64`.

- [ ] **Step 4: Implement the store side**

```go
// internal/hub/store/guardrails.go
package store

import (
	"database/sql"
	"errors"
	"time"
)

// GuardrailEvent is one transition of a lot 6 rule. It has no host: the
// subject is the budget, or a resource. Append-only, like alert_events.
type GuardrailEvent struct {
	ID      int64
	Subject string
	Rule    string
	Detail  string
	Kind    string
	Value   float64
	At      time.Time
}

// GuardrailKey names one rule instance: the 80 % threshold and the 100 %
// threshold are two instances of budget_threshold, with their own state.
type GuardrailKey struct{ Subject, Rule, Detail string }

const guardrailCols = `id, subject, rule, detail, kind, value, at`

func (s *Store) InsertGuardrailEvent(e GuardrailEvent) (GuardrailEvent, error) {
	res, err := s.db.Exec(`INSERT INTO azure_guardrail_events (subject, rule, detail, kind, value, at) VALUES (?,?,?,?,?,?)`,
		e.Subject, e.Rule, e.Detail, e.Kind, e.Value, e.At.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return GuardrailEvent{}, err
	}
	e.ID, _ = res.LastInsertId()
	return e, nil
}

func scanGuardrail(row interface{ Scan(...any) error }) (GuardrailEvent, error) {
	var e GuardrailEvent
	var at string
	if err := row.Scan(&e.ID, &e.Subject, &e.Rule, &e.Detail, &e.Kind, &e.Value, &at); err != nil {
		return GuardrailEvent{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return GuardrailEvent{}, err
	}
	e.At = t
	return e, nil
}

// LastGuardrailEventPerKey is what the evaluator compares against: the state
// of every rule instance is its most recent event.
func (s *Store) LastGuardrailEventPerKey() (map[GuardrailKey]GuardrailEvent, error) {
	rows, err := s.db.Query(`SELECT ` + guardrailCols + ` FROM azure_guardrail_events e
		WHERE id = (SELECT max(id) FROM azure_guardrail_events WHERE subject = e.subject AND rule = e.rule AND detail = e.detail)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[GuardrailKey]GuardrailEvent{}
	for rows.Next() {
		e, err := scanGuardrail(rows)
		if err != nil {
			return nil, err
		}
		out[GuardrailKey{e.Subject, e.Rule, e.Detail}] = e
	}
	return out, rows.Err()
}

func (s *Store) ListGuardrailEvents(limit int) ([]GuardrailEvent, error) {
	rows, err := s.db.Query(`SELECT `+guardrailCols+` FROM azure_guardrail_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GuardrailEvent
	for rows.Next() {
		e, err := scanGuardrail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateGuardrailDelivery(eventID int64, channel string, now time.Time) (Delivery, error) {
	ts := now.UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`INSERT INTO deliveries (guardrail_event_id, channel, state, created_at, updated_at) VALUES (?,?,'pending',?,?)`,
		eventID, channel, ts, ts)
	if err != nil {
		return Delivery{}, err
	}
	id, _ := res.LastInsertId()
	return s.Delivery(id)
}

// DeliveryGuardrailEvent answers ErrNotFound for a delivery that is about an
// alert event: the caller must ask the other accessor, not read a zero event.
func (s *Store) DeliveryGuardrailEvent(deliveryID int64) (GuardrailEvent, error) {
	var gid sql.NullInt64
	err := s.db.QueryRow(`SELECT guardrail_event_id FROM deliveries WHERE id = ?`, deliveryID).Scan(&gid)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !gid.Valid) {
		return GuardrailEvent{}, ErrNotFound
	}
	if err != nil {
		return GuardrailEvent{}, err
	}
	e, err := scanGuardrail(s.db.QueryRow(`SELECT `+guardrailCols+` FROM azure_guardrail_events WHERE id = ?`, gid.Int64))
	if errors.Is(err, sql.ErrNoRows) {
		return GuardrailEvent{}, ErrNotFound
	}
	return e, err
}
```

In `deliveries.go`: change `EventID int64` to `EventID *int64`, add `GuardrailEventID *int64`, set `deliveryCols` to `id, event_id, guardrail_event_id, channel, state, attempts, last_error, created_at, updated_at`, scan both with `sql.NullInt64`, and in `DeliveryEvent` return `ErrNotFound` when `event_id` is NULL. `CreateDelivery` is unchanged except that it now leaves `guardrail_event_id` NULL. Fix the two call sites that read `d.EventID` as an `int64` (`grep -rn '\.EventID' internal/`).

- [ ] **Step 5: Run the store tests**

Run: `go test ./internal/hub/store/ -race`
Expected: PASS, including the older delivery tests.

- [ ] **Step 6: Mutation check**

Remove the `CHECK` constraint from the migration, add `s.db.Exec("INSERT INTO deliveries (channel,state,created_at,updated_at) VALUES ('smtp','pending','x','x')")` to the second test expecting an error — see it now succeed where it must fail; restore the constraint (keep that assertion in the test as `TestADeliveryWithNoEventIsRefused`).

- [ ] **Step 7: Run the migration against a copy of the production database**

This migration drops and recreates a table that holds real rows, on a machine that has been
running for days, with `foreign_keys` on. Two migrations already reached the real database at once
in this project; nothing about that is hypothetical.

```bash
# from the Mac, with the NSG rule pointing at the current public IP
ssh azureuser@20.101.76.155 'sudo cp /var/lib/serversmonitor/serversmonitor.db /tmp/prod-copy.db && sudo chmod a+r /tmp/prod-copy.db'
scp azureuser@20.101.76.155:/tmp/prod-copy.db /tmp/prod-copy.db
ssh azureuser@20.101.76.155 'sudo rm -f /tmp/prod-copy.db'
go build -o /tmp/smhub-lot6 ./cmd/smhub
mkdir -p /tmp/sm-migrate && cp /tmp/prod-copy.db /tmp/sm-migrate/serversmonitor.db
SM_LISTEN=:8099 SM_DATA_DIR=/tmp/sm-migrate /tmp/smhub-lot6 &   # applies migration 6
sleep 3 && kill %1
```

Then, with a three-line Go program or `go test -run TestNothing` plus a temporary test that opens
`/tmp/sm-migrate/serversmonitor.db` with `store.Open`: assert `SELECT count(*) FROM deliveries`
equals what the copy held before (read it first with the old binary or `sqlite3`), that
`PendingDeliveries()` returns without error, and that `schema_version` is 6. Expected: identical
count, no error. If the count differs, the rebuild lost rows — stop and fix the migration.

- [ ] **Step 8: Commit**

```bash
git add internal/hub/store/migrations/0006_guardrails.sql internal/hub/store/guardrails.go internal/hub/store/guardrails_test.go internal/hub/store/deliveries.go
git commit -m "feat(store): guardrail events journal, deliveries that point at either journal"
```

---

### Task 2: Schedules, orphan columns and action origin in the store

**Files:**
- Create: `internal/hub/store/azure_schedules.go`, `internal/hub/store/azure_schedules_test.go`
- Modify: `internal/hub/store/azure.go` (`AzureResource` gains `OrphanReason string`, `OrphanSince *time.Time`; `ReplaceAzureInventory` and `ListAzureResources` carry them; new `SetAzureOrphans`)
- Modify: `internal/hub/store/azure_actions.go` (`AzureAction.Origin`, written by `StartAzureAction`, read by `ListAzureActions`)

**Interfaces:**
- Produces:
  ```go
  type AzureSchedule struct {
      ResourceID    string
      OffWindows    string // JSON, opaque to the store
      Enabled       bool
      LastBoundary  *time.Time
      LastAppliedAt *time.Time
      CreatedAt, UpdatedAt time.Time
  }
  func (s *Store) UpsertAzureSchedule(sc AzureSchedule, now time.Time) error
  func (s *Store) DeleteAzureSchedule(resourceID string) error
  func (s *Store) ListAzureSchedules() ([]AzureSchedule, error)
  // MarkScheduleBoundary advances last_boundary; it is called BEFORE the action.
  func (s *Store) MarkScheduleBoundary(resourceID string, boundary, now time.Time) error
  // SetAzureOrphans rewrites orphan_reason/orphan_since for every listed id and
  // clears them for every other live resource. since is kept when the reason
  // is unchanged, set to now when it changes from '' to something.
  func (s *Store) SetAzureOrphans(reasons map[string]string, now time.Time) error
  ```
  `AzureAction.Origin string` (`user` | `schedule`).

- [ ] **Step 1: Write the failing tests**

```go
// internal/hub/store/azure_schedules_test.go
package store

import (
	"testing"
	"time"
)

func TestSchedulesRoundTripAndBoundaryAdvances(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	if err := s.UpsertAzureSchedule(AzureSchedule{ResourceID: "/subscriptions/x/sites/a", OffWindows: `[{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]`, Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAzureSchedules()
	if err != nil || len(list) != 1 || list[0].LastBoundary != nil || !list[0].Enabled {
		t.Fatalf("list = %+v %v", list, err)
	}
	b := now.Add(-time.Hour)
	if err := s.MarkScheduleBoundary("/subscriptions/x/sites/a", b, now); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListAzureSchedules()
	if list[0].LastBoundary == nil || !list[0].LastBoundary.Equal(b) || list[0].LastAppliedAt == nil {
		t.Fatalf("boundary not recorded: %+v", list[0])
	}
	// Upsert keeps the boundary: editing the windows must not replay the last action.
	if err := s.UpsertAzureSchedule(AzureSchedule{ResourceID: "/subscriptions/x/sites/a", OffWindows: `[]`, Enabled: false}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListAzureSchedules()
	if list[0].Enabled || list[0].LastBoundary == nil {
		t.Fatalf("upsert lost state: %+v", list[0])
	}
	if err := s.DeleteAzureSchedule("/subscriptions/x/sites/a"); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListAzureSchedules(); len(list) != 0 {
		t.Fatal("not deleted")
	}
}

func TestOrphanSinceSurvivesASweepWithTheSameReason(t *testing.T) {
	s := openTest(t)
	t0 := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	rows := []AzureResource{{ID: "/s/disk1", ARMID: "/S/disk1", Name: "disk1", Type: "Microsoft.Compute/disks", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}
	if err := s.ReplaceAzureInventory([]string{"rg"}, rows, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAzureOrphans(map[string]string{"/s/disk1": "disk_unattached"}, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAzureResources()
	if r := byID(got, "/s/disk1"); r.OrphanReason != "disk_unattached" || r.OrphanSince == nil || !r.OrphanSince.Equal(t0) {
		t.Fatalf("first detection: %+v", r)
	}
	// A later sweep with the same reason keeps the original since.
	if err := s.ReplaceAzureInventory([]string{"rg"}, rows, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAzureOrphans(map[string]string{"/s/disk1": "disk_unattached"}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListAzureResources()
	if r := byID(got, "/s/disk1"); !r.OrphanSince.Equal(t0) {
		t.Fatalf("since moved: %+v", r)
	}
	// A different reason resets it; an absent id clears it.
	if err := s.SetAzureOrphans(map[string]string{"/s/site1": "unverified"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListAzureResources()
	if r := byID(got, "/s/disk1"); r.OrphanReason != "" || r.OrphanSince != nil {
		t.Fatalf("not cleared: %+v", r)
	}
	if r := byID(got, "/s/site1"); r.OrphanReason != "unverified" || !r.OrphanSince.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("not set: %+v", r)
	}
}

func byID(rs []AzureResource, id string) AzureResource {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return AzureResource{}
}

func TestActionOriginIsStoredAndListed(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	if err := s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/s/site1", ResourceName: "site1", Action: "stop", Origin: "schedule", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	as, _ := s.ListAzureActions(5)
	if len(as) != 1 || as[0].Origin != "schedule" {
		t.Fatalf("origin lost: %+v", as)
	}
	// Empty origin is written as "user", never as "".
	_ = s.FinishAzureAction(as[0].ID, "succeeded", "", nil, now)
	if _, err := s.StartAzureAction(AzureAction{ResourceID: "/s/site1", ResourceName: "site1", Action: "start", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	as, _ = s.ListAzureActions(5)
	if as[0].Origin != "user" {
		t.Fatalf("default origin = %q", as[0].Origin)
	}
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/store/ -run 'TestSchedules|TestOrphanSince|TestActionOrigin' -v`
Expected: FAIL — undefined `AzureSchedule`, `SetAzureOrphans`, unknown field `Origin`.

- [ ] **Step 3: Implement**

```go
// internal/hub/store/azure_schedules.go
package store

import (
	"database/sql"
	"time"
)

type AzureSchedule struct {
	ResourceID    string
	OffWindows    string
	Enabled       bool
	LastBoundary  *time.Time
	LastAppliedAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (s *Store) UpsertAzureSchedule(sc AzureSchedule, now time.Time) error {
	ts := now.UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO azure_schedules (resource_id, off_windows, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(resource_id) DO UPDATE SET off_windows = excluded.off_windows, enabled = excluded.enabled, updated_at = excluded.updated_at`,
		sc.ResourceID, sc.OffWindows, boolInt(sc.Enabled), ts, ts)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) DeleteAzureSchedule(resourceID string) error {
	_, err := s.db.Exec(`DELETE FROM azure_schedules WHERE resource_id = ?`, resourceID)
	return err
}

func (s *Store) ListAzureSchedules() ([]AzureSchedule, error) {
	rows, err := s.db.Query(`SELECT resource_id, off_windows, enabled, last_boundary, last_applied_at, created_at, updated_at FROM azure_schedules ORDER BY resource_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureSchedule
	for rows.Next() {
		var sc AzureSchedule
		var enabled int
		var lb, la sql.NullString
		var c, u string
		if err := rows.Scan(&sc.ResourceID, &sc.OffWindows, &enabled, &lb, &la, &c, &u); err != nil {
			return nil, err
		}
		sc.Enabled = enabled == 1
		sc.LastBoundary = parseNullTime(lb)
		sc.LastAppliedAt = parseNullTime(la)
		sc.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
		sc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, u)
		out = append(out, sc)
	}
	return out, rows.Err()
}

func parseNullTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}

// MarkScheduleBoundary is written before the action is issued, so that a
// crash between the two never replays the action (lot 4's asymmetry).
func (s *Store) MarkScheduleBoundary(resourceID string, boundary, now time.Time) error {
	_, err := s.db.Exec(`UPDATE azure_schedules SET last_boundary = ?, last_applied_at = ? WHERE resource_id = ?`,
		boundary.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), resourceID)
	return err
}
```

In `azure.go`: add `OrphanReason string` and `OrphanSince *time.Time` to `AzureResource`; read them in `ListAzureResources` (`orphan_reason`, `orphan_since` via `sql.NullString` + `parseNullTime`); leave `ReplaceAzureInventory` untouched except that its `INSERT … ON CONFLICT DO UPDATE` must **not** touch the two columns (they belong to `SetAzureOrphans`). Add:

```go
// SetAzureOrphans rewrites the orphan columns after a sweep. One transaction:
// a sweep that half-applied would show yesterday's orphans beside today's.
func (s *Store) SetAzureOrphans(reasons map[string]string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now.UTC().Format(time.RFC3339Nano)
	// Clear every live row that is not in the map, or whose reason changed.
	if _, err := tx.Exec(`UPDATE azure_resources SET orphan_reason = '', orphan_since = NULL WHERE deleted_at IS NULL`); err != nil {
		return err
	}
	for id, reason := range reasons {
		// COALESCE keeps the previous since only when the previous reason was
		// the same; the UPDATE above already cleared it, so we re-read the
		// pre-clear value with a subquery on a snapshot taken first.
		_ = id
		_ = reason
	}
	return tx.Commit()
}
```

The subquery approach above is a trap (the clear runs first). Implement it in two steps instead: (1) `SELECT id, orphan_reason, orphan_since FROM azure_resources WHERE orphan_reason <> ''` into a map **before** the clear; (2) clear; (3) for each `id, reason` in `reasons`: `since := ts; if prev, ok := before[id]; ok && prev.reason == reason && prev.since != "" { since = prev.since }`; `UPDATE azure_resources SET orphan_reason = ?, orphan_since = ? WHERE id = ?`. Write it that way; the snippet above is only the shape of the function.

In `azure_actions.go`: add `Origin string` to `AzureAction`; in `StartAzureAction`, `if a.Origin == "" { a.Origin = "user" }` and write the column; in `ListAzureActions` select and scan it.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/store/ -race`
Expected: PASS.

- [ ] **Step 5: Mutation check**

In `SetAzureOrphans`, drop the "same reason keeps since" branch (always `since = ts`); `TestOrphanSinceSurvivesASweepWithTheSameReason` must fail on "since moved". Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/store/
git commit -m "feat(store): schedules, orphan columns and action origin"
```

---

### Task 3: Guardrail settings

**Files:**
- Create: `internal/hub/guardrails/settings.go`, `internal/hub/guardrails/settings_test.go`
- Modify: `cmd/smhub/main.go` (add `_ "time/tzdata"` import)

**Interfaces:**
- Consumes: `azure.Getter`, `azure.Setter` (the two settings interfaces in `internal/hub/azure/config.go`).
- Produces:
  ```go
  package guardrails
  type Settings struct {
      Thresholds     []int  // percentages, ascending, deduplicated
      ResourceShare  int    // percent of the budget
      HubVMSilentDays int
      Timezone       string
  }
  func DefaultSettings() Settings // {80,100}, 30, 3, "Europe/Paris"
  func LoadSettings(g azure.Getter) Settings
  func SaveSettings(s azure.Setter, c Settings) error
  func (c Settings) Validate() error
  func (c Settings) Location() *time.Location // never nil: falls back to UTC and the caller logs
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/hub/guardrails/settings_test.go
package guardrails

import (
	"strings"
	"testing"
)

type mem map[string]string

func (m mem) GetSetting(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }
func (m mem) SetSetting(k, v string) error            { m[k] = v; return nil }

func TestDefaultsAndRoundTrip(t *testing.T) {
	m := mem{}
	c := LoadSettings(m)
	if got, want := c, DefaultSettings(); got.ResourceShare != want.ResourceShare || got.Timezone != "Europe/Paris" || len(got.Thresholds) != 2 {
		t.Fatalf("defaults = %+v", got)
	}
	c.Thresholds = []int{100, 50, 50}
	c.ResourceShare = 40
	c.HubVMSilentDays = 7
	c.Timezone = "UTC"
	if err := SaveSettings(m, c); err != nil {
		t.Fatal(err)
	}
	back := LoadSettings(m)
	if strings.Join(itoa(back.Thresholds), ",") != "50,100" || back.ResourceShare != 40 || back.HubVMSilentDays != 7 || back.Timezone != "UTC" {
		t.Fatalf("round trip = %+v", back)
	}
	if m["azure_budget_thresholds"] != "50,100" {
		t.Fatalf("stored as %q", m["azure_budget_thresholds"])
	}
}

func TestValidateRefusesWhatTheSpecRefuses(t *testing.T) {
	cases := map[string]Settings{
		"threshold 0":     {Thresholds: []int{0}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "UTC"},
		"threshold 501":   {Thresholds: []int{501}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "UTC"},
		"share 0":         {Thresholds: []int{80}, ResourceShare: 0, HubVMSilentDays: 3, Timezone: "UTC"},
		"share 101":       {Thresholds: []int{80}, ResourceShare: 101, HubVMSilentDays: 3, Timezone: "UTC"},
		"silent 0":        {Thresholds: []int{80}, ResourceShare: 30, HubVMSilentDays: 0, Timezone: "UTC"},
		"zone unknown":    {Thresholds: []int{80}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "Mars/Olympus"},
	}
	for name, c := range cases {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "zone unknown" && !strings.Contains(err.Error(), "Mars/Olympus") {
			t.Errorf("the message must name the zone: %v", err)
		}
	}
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
}

func TestLocationNeverNil(t *testing.T) {
	// The parentheses are required: Go refuses an unparenthesised composite
	// literal at the head of an if condition.
	if (Settings{Timezone: "Nowhere/Nowhere"}).Location() != time.UTC {
		t.Fatal("unknown zone must fall back to UTC")
	}
	if (Settings{Timezone: "Europe/Paris"}).Location().String() != "Europe/Paris" {
		t.Fatal("known zone")
	}
}

func itoa(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}
```
(add `"strconv"` and `"time"` to the imports.)

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/guardrails/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

```go
// internal/hub/guardrails/settings.go
package guardrails

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
)

type Settings struct {
	Thresholds      []int
	ResourceShare   int
	HubVMSilentDays int
	Timezone        string
}

func DefaultSettings() Settings {
	return Settings{Thresholds: []int{80, 100}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "Europe/Paris"}
}

func LoadSettings(g azure.Getter) Settings {
	d := DefaultSettings()
	get := func(k, def string) string {
		v, ok, err := g.GetSetting(k)
		if err != nil || !ok || v == "" {
			return def
		}
		return v
	}
	c := Settings{Timezone: get("azure_timezone", d.Timezone)}
	c.Thresholds = parseThresholds(get("azure_budget_thresholds", "80,100"))
	if len(c.Thresholds) == 0 {
		c.Thresholds = d.Thresholds
	}
	c.ResourceShare = atoiOr(get("azure_resource_share_pct", ""), d.ResourceShare)
	c.HubVMSilentDays = atoiOr(get("azure_hub_vm_silent_days", ""), d.HubVMSilentDays)
	return c
}

func parseThresholds(s string) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func SaveSettings(s azure.Setter, c Settings) error {
	c.Thresholds = parseThresholds(strings.Join(itoa(c.Thresholds), ","))
	for k, v := range map[string]string{
		"azure_budget_thresholds":  strings.Join(itoa(c.Thresholds), ","),
		"azure_resource_share_pct": strconv.Itoa(c.ResourceShare),
		"azure_hub_vm_silent_days": strconv.Itoa(c.HubVMSilentDays),
		"azure_timezone":           c.Timezone,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

func itoa(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}

func (c Settings) Validate() error {
	if len(c.Thresholds) == 0 {
		return errors.New("at least one budget threshold is required")
	}
	for _, t := range c.Thresholds {
		if t < 1 || t > 500 {
			return fmt.Errorf("a budget threshold must be between 1 and 500 %%, got %d", t)
		}
	}
	if c.ResourceShare < 1 || c.ResourceShare > 100 {
		return fmt.Errorf("the resource share must be between 1 and 100 %%, got %d", c.ResourceShare)
	}
	if c.HubVMSilentDays < 1 {
		return errors.New("the silent-VM age must be at least one day")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "" {
		return fmt.Errorf("unknown time zone %q; use an IANA name such as Europe/Paris", c.Timezone)
	}
	return nil
}

// Location never returns nil. A zone that vanished after being saved is a
// runtime oddity (the zone database is embedded), and the caller logs it.
func (c Settings) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil || c.Timezone == "" {
		return time.UTC
	}
	return loc
}
```

The test file defines its own `itoa`; delete the test copy since the package now has one. In `cmd/smhub/main.go` add `_ "time/tzdata"` to the imports with the comment `// the runtime image is FROM scratch and has no zone database`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/guardrails/ -race -v`
Expected: PASS.

- [ ] **Step 5: Mutation check**

Change `t > 500` to `t > 5000`; `TestValidateRefusesWhatTheSpecRefuses` fails on "threshold 501: accepted". Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/guardrails/ cmd/smhub/main.go
git commit -m "feat(guardrails): settings with validation, zone database embedded"
```

---

### Task 4: Budget evaluator (pure)

**Files:**
- Create: `internal/hub/guardrails/budget.go`, `internal/hub/guardrails/budget_test.go`

**Interfaces:**
- Consumes: `store.GuardrailEvent`, `store.GuardrailKey`.
- Produces:
  ```go
  type CostRow struct{ ResourceID, Name string; Amount float64 }
  type BudgetInput struct {
      Now        time.Time          // UTC
      Budget     float64            // 0 disables every rule
      Spent      float64            // all currencies summed
      Currencies int                // how many; the message says when > 1
      AsOf       time.Time          // the latest cost as_of; days billed = AsOf.Day() - 1 in the month of Now (see below)
      Rows       []CostRow          // per resource, month to date
      Settings   Settings
      Last       map[store.GuardrailKey]store.GuardrailEvent
  }
  // Budget returns the transitions to journal, nothing else.
  func Budget(in BudgetInput) []store.GuardrailEvent
  // Projection returns the end-of-month projection and false before the 4th billed day.
  func Projection(now, asOf time.Time, spent float64) (float64, bool)
  ```
  Days billed: Cost Management's month-to-date figure on `as_of` covers the days **before** `as_of`'s date, so `daysBilled = asOf.In(UTC).Day() - 1` when `asOf` is in the same month as `now`; if `asOf` is in an earlier month (nothing billed yet this month) it is 0. Days in month from `time.Date(y, m+1, 0, …).Day()`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/hub/guardrails/budget_test.go
package guardrails

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func day(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }

func keys(evs []store.GuardrailEvent) map[string]string {
	out := map[string]string{}
	for _, e := range evs {
		out[e.Subject+"|"+e.Rule+"|"+e.Detail] = e.Kind
	}
	return out
}

func TestThresholdsFireOnceEach(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 100, Spent: 85, AsOf: day(20), Settings: DefaultSettings(), Last: map[store.GuardrailKey]store.GuardrailEvent{}}
	got := keys(Budget(in))
	if got["budget|budget_threshold|80"] != "fired" || got["budget|budget_threshold|100"] != "" {
		t.Fatalf("got %v", got)
	}
	// Same input again, with the journal holding the fired event: nothing.
	in.Last[store.GuardrailKey{Subject: "budget", Rule: "budget_threshold", Detail: "80"}] = store.GuardrailEvent{Kind: "fired"}
	if evs := Budget(in); len(keys(evs)) != 0 && keys(evs)["budget|budget_threshold|80"] != "" {
		t.Fatalf("re-fired: %v", keys(evs))
	}
}

func TestNewMonthResolvesEverything(t *testing.T) {
	last := map[store.GuardrailKey]store.GuardrailEvent{
		{"budget", "budget_threshold", "80"}:  {Kind: "fired"},
		{Subject: "budget", Rule: "budget_threshold", Detail: "100"}: {Kind: "fired"},
		{"budget", "budget_projection", ""}:   {Kind: "fired"},
		{"/s/vm1", "resource_share", ""}:      {Kind: "fired"},
	}
	in := BudgetInput{Now: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC), Budget: 100, Spent: 0, AsOf: time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), Settings: DefaultSettings(), Last: last}
	got := keys(Budget(in))
	for _, k := range []string{"budget|budget_threshold|80", "budget|budget_threshold|100", "budget|budget_projection|", "/s/vm1|resource_share|"} {
		if got[k] != "resolved" {
			t.Fatalf("%s = %q, want resolved (%v)", k, got[k], got)
		}
	}
}

func TestProjectionWaitsForFourBilledDaysAndHasHysteresis(t *testing.T) {
	if _, ok := Projection(day(4), day(4), 10); ok { // as_of the 4th → 3 billed days
		t.Fatal("3 billed days must not project")
	}
	p, ok := Projection(day(5), day(5), 10) // 4 billed days, 30-day month → 75
	if !ok || p < 74.9 || p > 75.1 {
		t.Fatalf("projection = %v %v", p, ok)
	}
	base := BudgetInput{Now: day(11), Budget: 100, AsOf: day(11), Settings: DefaultSettings()}
	// 10 billed days. Spent 34 → 102: inside the 5 % band, no fire.
	in := base
	in.Spent, in.Last = 34, map[store.GuardrailKey]store.GuardrailEvent{}
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "" {
		t.Fatalf("fired inside the band: %v", k)
	}
	// Spent 36 → 108 > 105: fires.
	in.Spent = 36
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "fired" {
		t.Fatalf("did not fire: %v", k)
	}
	// Firing, spent 33 → 99: still above 95, no resolve.
	in.Spent, in.Last = 33, map[store.GuardrailKey]store.GuardrailEvent{{Subject: "budget", Rule: "budget_projection", Detail: ""}: {Kind: "fired"}}
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "" {
		t.Fatalf("resolved inside the band: %v", k)
	}
	// Spent 31 → 93 < 95: resolves.
	in.Spent = 31
	if k := keys(Budget(in)); k["budget|budget_projection|"] != "resolved" {
		t.Fatalf("did not resolve: %v", k)
	}
}

func TestResourceShareIsPerResourceAndCountsDeletedOnes(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 100, Spent: 60, AsOf: day(20), Settings: DefaultSettings(), Last: map[store.GuardrailKey]store.GuardrailEvent{},
		Rows: []CostRow{{ResourceID: "/s/vm1", Name: "vm1", Amount: 45}, {ResourceID: "/s/gone", Name: "gone", Amount: 31}, {ResourceID: "/s/site", Name: "site", Amount: 2}}}
	got := keys(Budget(in))
	if got["/s/vm1|resource_share|"] != "fired" || got["/s/gone|resource_share|"] != "fired" || got["/s/site|resource_share|"] != "" {
		t.Fatalf("got %v", got)
	}
	// The value carried is the share in percent.
	for _, e := range Budget(in) {
		if e.Subject == "/s/vm1" && (e.Value < 44.9 || e.Value > 45.1) {
			t.Fatalf("value = %v, want the share 45", e.Value)
		}
	}
}

func TestBudgetZeroDisablesAndResolves(t *testing.T) {
	in := BudgetInput{Now: day(20), Budget: 0, Spent: 900, AsOf: day(20), Settings: DefaultSettings(),
		Last: map[store.GuardrailKey]store.GuardrailEvent{{Subject: "budget", Rule: "budget_threshold", Detail: "80"}: {Kind: "fired"}}}
	got := keys(Budget(in))
	if len(got) != 1 || got["budget|budget_threshold|80"] != "resolved" {
		t.Fatalf("got %v", got)
	}
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/guardrails/ -run 'Threshold|NewMonth|Projection|ResourceShare|BudgetZero' -v`
Expected: FAIL — undefined `BudgetInput`, `Budget`, `Projection`.

- [ ] **Step 3: Implement**

```go
// internal/hub/guardrails/budget.go
package guardrails

import (
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type CostRow struct {
	ResourceID string
	Name       string
	Amount     float64
}

type BudgetInput struct {
	Now        time.Time
	Budget     float64
	Spent      float64
	Currencies int
	AsOf       time.Time
	Rows       []CostRow
	Settings   Settings
	Last       map[store.GuardrailKey]store.GuardrailEvent
}

const projectionBand = 0.05

// budgetRules names what a budget of 0 turns off. Positive, not an exclusion
// list: a rule added later must not be resolved by a branch that never heard
// of it.
var budgetRules = map[string]bool{"budget_threshold": true, "budget_projection": true, "resource_share": true}

// daysBilled is how many days of the current month Cost Management has
// actually billed on the as_of date: the figure dated the 5th covers the 1st
// to the 4th. Counting today would understate the pace by a day.
func daysBilled(now, asOf time.Time) int {
	now, asOf = now.UTC(), asOf.UTC()
	if asOf.Year() != now.Year() || asOf.Month() != now.Month() {
		return 0
	}
	return asOf.Day() - 1
}

func daysInMonth(t time.Time) int {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Projection is the rule of three, and it refuses to guess on fewer than
// four billed days.
func Projection(now, asOf time.Time, spent float64) (float64, bool) {
	d := daysBilled(now, asOf)
	if d < 4 {
		return 0, false
	}
	return spent * float64(daysInMonth(now)) / float64(d), true
}

// want records the wanted state of one rule instance; Budget turns wants into
// transitions by comparing them with the journal.
type want struct {
	key   store.GuardrailKey
	on    bool
	value float64
}

func Budget(in BudgetInput) []store.GuardrailEvent {
	var wants []want
	if in.Budget > 0 {
		for _, pct := range in.Settings.Thresholds {
			line := in.Budget * float64(pct) / 100
			wants = append(wants, want{key: store.GuardrailKey{Subject: "budget", Rule: "budget_threshold", Detail: strconv.Itoa(pct)}, on: in.Spent >= line, value: in.Spent})
		}
		pkey := store.GuardrailKey{Subject: "budget", Rule: "budget_projection"}
		if p, ok := Projection(in.Now, in.AsOf, in.Spent); ok {
			firing := in.Last[pkey].Kind == "fired"
			on := p > in.Budget*(1+projectionBand)
			if firing {
				on = p >= in.Budget*(1-projectionBand) // stays on inside the band
			}
			wants = append(wants, want{key: pkey, on: on, value: p})
		} else if in.Last[pkey].Kind == "fired" {
			wants = append(wants, want{key: pkey, on: false, value: 0}) // new month: resolve
		}
		shareLine := in.Budget * float64(in.Settings.ResourceShare) / 100
		seen := map[string]bool{}
		for _, r := range in.Rows {
			seen[r.ResourceID] = true
			wants = append(wants, want{key: store.GuardrailKey{Subject: r.ResourceID, Rule: "resource_share"}, on: r.Amount > shareLine, value: r.Amount / in.Budget * 100})
		}
		// A resource that fired last month and has no row this month resolves.
		for k, e := range in.Last {
			if k.Rule == "resource_share" && e.Kind == "fired" && !seen[k.Subject] {
				wants = append(wants, want{key: k, on: false})
			}
		}
	} else {
		// A budget of 0 disables the budget rules and resolves them. The list
		// is positive on purpose: excluding today's non-budget rules by name
		// would silently resolve any rule added later.
		for k, e := range in.Last {
			if e.Kind == "fired" && budgetRules[k.Rule] {
				wants = append(wants, want{key: k, on: false})
			}
		}
	}
	return transitions(wants, in.Last, in.Now)
}

// transitions is the one place a wanted state becomes a journal line: only
// when it differs from the last line for that instance.
func transitions(wants []want, last map[store.GuardrailKey]store.GuardrailEvent, now time.Time) []store.GuardrailEvent {
	var out []store.GuardrailEvent
	for _, w := range wants {
		firing := last[w.key].Kind == "fired"
		switch {
		case w.on && !firing:
			out = append(out, store.GuardrailEvent{Subject: w.key.Subject, Rule: w.key.Rule, Detail: w.key.Detail, Kind: "fired", Value: w.value, At: now})
		case !w.on && firing:
			out = append(out, store.GuardrailEvent{Subject: w.key.Subject, Rule: w.key.Rule, Detail: w.key.Detail, Kind: "resolved", Value: w.value, At: now})
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/guardrails/ -race -v`
Expected: PASS.

- [ ] **Step 5: Mutation check**

In `transitions`, change `case w.on && !firing` to `case w.on`; `TestThresholdsFireOnceEach` fails on "re-fired". Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/guardrails/budget.go internal/hub/guardrails/budget_test.go
git commit -m "feat(guardrails): budget thresholds, projection with hysteresis, resource share"
```

---

### Task 5: Orphan typed reads in the azure package

**Files:**
- Create: `internal/hub/azure/orphans.go`, `internal/hub/azure/orphans_test.go`

**Interfaces:**
- Consumes: `Client.Get`, `StatusOf`, `Resource`.
- Produces:
  ```go
  const (
      OrphanDiskUnattached = "disk_unattached"
      OrphanIPUnassociated = "ip_unassociated"
      OrphanNICWithoutVM   = "nic_without_vm"
      OrphanPlanWithoutSite = "plan_without_site"
      OrphanHubVMSilent    = "hub_vm_silent"   // decided by the hub, not here
      OrphanUnverified     = "unverified"
  )
  // OrphanReasons runs one typed read per resource of a type it knows and
  // returns the reason per lowercased id. Absent = healthy or unknown type.
  // 403 → OrphanUnverified; 404 → absent; any other error aborts the sweep.
  func OrphanReasons(ctx context.Context, c *Client, rs []Resource) (map[string]string, error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/hub/azure/orphans_test.go
package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shapes copied from `az disk show`, `az network public-ip show`, `az network
// nic show`, `az appservice plan show` on 2026-09-23, trimmed to the fields read.
const (
	diskUnattached = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1","properties":{"diskState":"Unattached","diskSizeGB":32}}`
	diskAttached   = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d2","properties":{"diskState":"Attached","diskSizeGB":30}}`
	ipFree         = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1","properties":{"ipAddress":"20.1.2.3","publicIPAllocationMethod":"Static"}}`
	ipUsed         = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip2","properties":{"ipConfiguration":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1/ipConfigurations/ipconfig1"}}}`
	nicFree        = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n0","properties":{"ipConfigurations":[]}}`
	nicUsed        = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1","properties":{"virtualMachine":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"}}}`
	planEmpty      = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p0","properties":{"numberOfSites":0,"status":"Ready"}}`
	planUsed       = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p1","properties":{"numberOfSites":2,"status":"Ready"}}`
)

func orphanFake(t *testing.T, bodies map[string]string, status map[string]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		for suffix, code := range status {
			if strings.HasSuffix(r.URL.Path, suffix) {
				w.WriteHeader(code)
				io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"no"}}`)
				return
			}
		}
		for suffix, body := range bodies {
			if strings.HasSuffix(r.URL.Path, suffix) {
				io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"ResourceNotFound","message":"gone"}}`)
	}))
}

func res(armID, typ string) Resource {
	return Resource{ID: NormalizeID(armID), ARMID: armID, Type: typ, Name: armID[strings.LastIndex(armID, "/")+1:]}
}

func TestEachDetectorReadsAzuresOwnShape(t *testing.T) {
	srv := orphanFake(t, map[string]string{"/d1": diskUnattached, "/d2": diskAttached, "/ip1": ipFree, "/ip2": ipUsed, "/n0": nicFree, "/n1": nicUsed, "/p0": planEmpty, "/p1": planUsed}, nil)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs := []Resource{
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d2", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1", "Microsoft.Network/publicIPAddresses"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip2", "Microsoft.Network/publicIPAddresses"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n0", "Microsoft.Network/networkInterfaces"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1", "Microsoft.Network/networkInterfaces"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p0", "Microsoft.Web/serverfarms"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p1", "Microsoft.Web/serverfarms"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/site1", "Microsoft.Web/sites"), // unknown to the detectors: no read at all
	}
	got, err := OrphanReasons(context.Background(), c, rs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		NormalizeID(rs[0].ARMID): OrphanDiskUnattached,
		NormalizeID(rs[2].ARMID): OrphanIPUnassociated,
		NormalizeID(rs[4].ARMID): OrphanNICWithoutVM,
		NormalizeID(rs[6].ARMID): OrphanPlanWithoutSite,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestA403IsUnverifiedAndA404IsSkipped(t *testing.T) {
	srv := orphanFake(t, map[string]string{"/d1": diskUnattached}, map[string]int{"/d3": 403})
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs := []Resource{
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d3", "Microsoft.Compute/disks"), // 403
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d4", "Microsoft.Compute/disks"), // 404
	}
	got, err := OrphanReasons(context.Background(), c, rs)
	if err != nil {
		t.Fatal(err)
	}
	if got[NormalizeID(rs[1].ARMID)] != OrphanUnverified {
		t.Fatalf("403 must be unverified: %v", got)
	}
	if _, ok := got[NormalizeID(rs[2].ARMID)]; ok {
		t.Fatalf("404 must be absent: %v", got)
	}
}

func TestAnyOtherErrorAbortsTheSweep(t *testing.T) {
	srv := orphanFake(t, nil, map[string]int{"/d1": 500})
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: func(context.Context, time.Duration) bool { return true }})
	_, err := OrphanReasons(context.Background(), c, []Resource{res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks")})
	if err == nil {
		t.Fatal("a 500 is not an answer about orphans")
	}
}
```
(`staticSource` exists in `client_test.go`; add `"time"` to the imports.)

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/azure/ -run 'Detector|Unverified|AbortsTheSweep' -v`
Expected: FAIL — undefined `OrphanReasons`.

- [ ] **Step 3: Implement**

```go
// internal/hub/azure/orphans.go
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	OrphanDiskUnattached  = "disk_unattached"
	OrphanIPUnassociated  = "ip_unassociated"
	OrphanNICWithoutVM    = "nic_without_vm"
	OrphanPlanWithoutSite = "plan_without_site"
	OrphanHubVMSilent     = "hub_vm_silent"
	OrphanUnverified      = "unverified"
)

const diskAPIVersion = "2024-03-02"

// detector reads one resource and says whether it is an orphan. The body is
// Azure's own JSON; each detector reads the one property that decides.
type detector struct {
	apiVersion string
	reason     string
	orphan     func(body []byte) (bool, error)
}

var detectors = map[string]detector{
	"microsoft.compute/disks": {diskAPIVersion, OrphanDiskUnattached, func(b []byte) (bool, error) {
		var v struct{ Properties struct{ DiskState string `json:"diskState"` } }
		return unmarshalProp(b, &v, func() bool { return v.Properties.DiskState == "Unattached" })
	}},
	"microsoft.network/publicipaddresses": {networkAPIVersion, OrphanIPUnassociated, func(b []byte) (bool, error) {
		var v struct{ Properties struct{ IPConfiguration *struct{ ID string } `json:"ipConfiguration"` } }
		return unmarshalProp(b, &v, func() bool { return v.Properties.IPConfiguration == nil })
	}},
	"microsoft.network/networkinterfaces": {networkAPIVersion, OrphanNICWithoutVM, func(b []byte) (bool, error) {
		var v struct{ Properties struct{ VirtualMachine *struct{ ID string } `json:"virtualMachine"` } }
		return unmarshalProp(b, &v, func() bool { return v.Properties.VirtualMachine == nil })
	}},
	"microsoft.web/serverfarms": {webAPIVersion, OrphanPlanWithoutSite, func(b []byte) (bool, error) {
		var v struct{ Properties struct{ NumberOfSites *int `json:"numberOfSites"` } }
		return unmarshalProp(b, &v, func() bool { return v.Properties.NumberOfSites != nil && *v.Properties.NumberOfSites == 0 })
	}},
}

func unmarshalProp(b []byte, v any, decide func() bool) (bool, error) {
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("azure: orphan read did not parse: %w", err)
	}
	return decide(), nil
}

// OrphanReasons decides, per resource of a type it knows, whether it is an
// orphan. A 403 is recorded as unverified rather than silently healthy: the
// role may not cover the read, and "not verified" is the truthful answer.
// A 404 is skipped: the resource left between the catalogue and this read.
// Any other failure aborts the sweep, as a partial inventory would.
func OrphanReasons(ctx context.Context, c *Client, rs []Resource) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range rs {
		d, ok := detectors[strings.ToLower(r.Type)]
		if !ok {
			continue
		}
		body, err := c.Get(ctx, r.ARMID, url.Values{"api-version": {d.apiVersion}})
		switch StatusOf(err) {
		case 0:
		case http.StatusForbidden:
			out[r.ID] = OrphanUnverified
			continue
		case http.StatusNotFound:
			continue
		default:
			return nil, fmt.Errorf("orphan read on %s: %w", r.Name, err)
		}
		if err != nil { // a transport error has no status
			return nil, fmt.Errorf("orphan read on %s: %w", r.Name, err)
		}
		orphan, err := d.orphan(body)
		if err != nil {
			return nil, err
		}
		if orphan {
			out[r.ID] = d.reason
		}
	}
	return out, nil
}
```

Check what `StatusOf` returns for a nil error and for a non-ARM error (read `client.go:279`); adjust the `switch` so that a nil error falls through to parsing and a transport error aborts.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/azure/ -race`
Expected: PASS.

- [ ] **Step 5: Mutation check**

Make the 403 case `continue` without setting `OrphanUnverified`; `TestA403IsUnverifiedAndA404IsSkipped` fails. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/azure/orphans.go internal/hub/azure/orphans_test.go
git commit -m "feat(azure): typed orphan reads — unattached disks, free IPs, NICs without VM, empty plans"
```

---

### Task 6: Orphan evaluator (pure) and the hub wiring for budget and orphans

**Files:**
- Create: `internal/hub/guardrails/orphans.go`, `internal/hub/guardrails/orphans_test.go`
- Create: `internal/hub/guardrails_hub.go`, `internal/hub/guardrails_hub_test.go`
- Modify: `internal/hub/azure_sync.go` (call the evaluators after each successful sync)
- Modify: `internal/hub/notify.go` (guardrail messages; replay of guardrail deliveries)
- Modify: `internal/hub/server/api_notify.go:194-196` (deliveries of guardrail events show the subject)

**Interfaces:**
- Produces (pure):
  ```go
  type OrphanInput struct {
      Now       time.Time
      Reasons   map[string]string      // from azure.OrphanReasons, plus hub_vm_silent added by the hub
      Names     map[string]string      // id → name, for messages
      Last      map[store.GuardrailKey]store.GuardrailEvent
  }
  // Orphans returns the transitions: fired for a new reason (except unverified),
  // resolved for a resource whose reason is gone or is now unverified.
  func Orphans(in OrphanInput) []store.GuardrailEvent
  // HubVMSilent decides the fifth reason from hub data.
  func HubVMSilent(hostStatus string, lastSeen *time.Time, now time.Time, silentDays int) bool
  ```
- Produces (hub): `func (h *Hub) evaluateBudget()`, `func (h *Hub) evaluateOrphans(ctx, client, resources)`, `func (h *Hub) journalOrphans(reasons map[string]string)`, `func (h *Hub) journalGuardrails(evs []store.GuardrailEvent)`, `func (h *Hub) GuardrailSettings() guardrails.Settings`.

**The orphan sweep has its own sync scope.** A 500 on one disk read must not blank the inventory's
sync state: the catalogue and enrichment passes succeeded, and the page's red banner would claim
the inventory is stale when it is not. `azure_sync` is keyed by scope and the page renders it as a
map, so a third scope `orphans` needs no schema change and no front change beyond a label. When the
orphan pass fails, `SetAzureOrphans` is not called at all: yesterday's reasons stay on the rows
rather than resolving into a false "no orphans", which is the same rule as every other guardrail.

- [ ] **Step 1: Write the failing pure tests**

```go
// internal/hub/guardrails/orphans_test.go
package guardrails

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func TestOrphansFireOnceAndResolveWhenTheReasonGoes(t *testing.T) {
	now := day(20)
	in := OrphanInput{Now: now, Reasons: map[string]string{"/s/d1": "disk_unattached", "/s/ip1": "unverified"}, Names: map[string]string{"/s/d1": "d1"}, Last: map[store.GuardrailKey]store.GuardrailEvent{}}
	got := keys(Orphans(in))
	if got["/s/d1|orphan|"] != "fired" || len(got) != 1 {
		t.Fatalf("unverified must not fire: %v", got)
	}
	in.Last[store.GuardrailKey{Subject: "/s/d1", Rule: "orphan", Detail: ""}] = store.GuardrailEvent{Kind: "fired"}
	if got := keys(Orphans(in)); len(got) != 0 {
		t.Fatalf("re-fired: %v", got)
	}
	// Reason gone → resolved. Reason now unverified → resolved too (we no longer know).
	in.Reasons = map[string]string{"/s/d1": "unverified"}
	if got := keys(Orphans(in)); got["/s/d1|orphan|"] != "resolved" {
		t.Fatalf("got %v", got)
	}
}

func TestHubVMSilent(t *testing.T) {
	now := day(20)
	old := day(10)
	recent := day(19)
	if !HubVMSilent("never_seen", nil, now, 3) {
		t.Fatal("never seen is silent")
	}
	if !HubVMSilent("offline", &old, now, 3) || HubVMSilent("offline", &recent, now, 3) {
		t.Fatal("offline: silent only past the age")
	}
	if HubVMSilent("online", &old, now, 3) {
		t.Fatal("online is never silent")
	}
}
```

- [ ] **Step 2: Implement the pure part**

```go
// internal/hub/guardrails/orphans.go
package guardrails

import (
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type OrphanInput struct {
	Now     time.Time
	Reasons map[string]string
	Names   map[string]string
	Last    map[store.GuardrailKey]store.GuardrailEvent
}

func Orphans(in OrphanInput) []store.GuardrailEvent {
	var wants []want
	seen := map[string]bool{}
	for id, reason := range in.Reasons {
		seen[id] = true
		wants = append(wants, want{key: store.GuardrailKey{Subject: id, Rule: "orphan"}, on: reason != "" && reason != "unverified"})
	}
	for k, e := range in.Last {
		if k.Rule == "orphan" && e.Kind == "fired" && !seen[k.Subject] {
			wants = append(wants, want{key: k, on: false})
		}
	}
	return transitions(wants, in.Last, in.Now)
}

// HubVMSilent is the one reason the hub decides from its own data: a VM it
// created whose agent never reported, or stopped reporting long ago.
func HubVMSilent(hostStatus string, lastSeen *time.Time, now time.Time, silentDays int) bool {
	switch hostStatus {
	case "never_seen":
		return true
	case "offline":
		return lastSeen == nil || now.Sub(*lastSeen) > time.Duration(silentDays)*24*time.Hour
	}
	return false
}
```

Run: `go test ./internal/hub/guardrails/ -race` — PASS.

- [ ] **Step 3: Write the failing hub test**

Read `internal/hub/azure_test.go` first: `newAzureFake`, `newTestHub` (or whatever the fixture is called at line 165) and how a fake ARM answers `/resources` and the cost query. Extend the fake with a `disks []string` list answered on the catalogue (type `Microsoft.Compute/disks`) and on `GET …/disks/<name>` with `{"properties":{"diskState":"Unattached"}}`; a `diskReadStatus int` that makes that typed read answer with an error when non-zero; `costs map[string]float64` answered by the Cost Management query; and `id(name string) string` returning the lowercased ARM id the fake serves for that name.

```go
// internal/hub/guardrails_hub_test.go
package hub

import (
	"testing"
	"time"
)

func TestASuccessfulSyncJournalsGuardrailTransitionsOnce(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"}          // unattached
	f.costs = map[string]float64{"site1": 45, "d1": 5} // month to date, EUR
	defer f.srv.Close()
	h := newTestHub(t, f) // the fixture used by TestAzureSyncStoresInventoryAndState
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	h.ReloadAzure()

	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())

	last, _ := h.st.LastGuardrailEventPerKey()
	if last[store.GuardrailKey{Subject: f.id("d1"), Rule: "orphan"}].Kind != "fired" {
		t.Fatalf("orphan not journaled: %v", last)
	}
	if last[store.GuardrailKey{Subject: f.id("site1"), Rule: "resource_share"}].Kind != "fired" {
		t.Fatalf("share not journaled: %v", last)
	}
	n := len(last)
	// Same answers again: nothing new.
	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())
	evs, _ := h.st.ListGuardrailEvents(100)
	if len(evs) != n {
		t.Fatalf("journal grew from %d to %d on identical input", n, len(evs))
	}
	// The inventory carries the reason.
	rs, _ := h.st.ListAzureResources()
	if r := byID(rs, f.id("d1")); r.OrphanReason != "disk_unattached" || r.OrphanSince == nil {
		t.Fatalf("inventory: %+v", r)
	}
}

func TestAFailedOrphanReadLeavesTheInventoryGreenAndTheReasonsAlone(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"}
	defer f.srv.Close()
	h := newTestHub(t, f)
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	if r := byID(mustList(t, h), f.id("d1")); r.OrphanReason != "disk_unattached" {
		t.Fatalf("first sweep: %+v", r)
	}
	f.diskReadStatus = 500 // the typed read fails; the catalogue still answers
	h.syncAzureInventory(context.Background())
	st, _ := h.st.AzureSyncState()
	if !st["inventory"].OK {
		t.Fatalf("the inventory pass succeeded and must stay green: %+v", st["inventory"])
	}
	if st["orphans"].OK || st["orphans"].Message == "" {
		t.Fatalf("the orphan scope must carry the failure: %+v", st["orphans"])
	}
	if r := byID(mustList(t, h), f.id("d1")); r.OrphanReason != "disk_unattached" {
		t.Fatalf("a failed sweep must not clear yesterday's reasons: %+v", r)
	}
	if evs, _ := h.st.ListGuardrailEvents(10); len(evs) != 1 { // only the first fired event
		t.Fatalf("a failed sweep must not journal: %+v", evs)
	}
}

func mustList(t *testing.T, h *Hub) []store.AzureResource {
	t.Helper()
	rs, err := h.st.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestAFailedSyncEvaluatesNothing(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.costs = map[string]float64{"site1": 95}
	defer f.srv.Close()
	h := newTestHub(t, f)
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	h.ReloadAzure()
	f.failAfter = 1 // the token call succeeds, the first ARM call fails
	h.syncAzureCosts(context.Background())
	if evs, _ := h.st.ListGuardrailEvents(10); len(evs) != 0 {
		t.Fatalf("a failed sync must not journal: %+v", evs)
	}
}

func TestGuardrailTransitionsAreDeliveredAndReplayed(t *testing.T) {
	// A webhook channel that records what it receives; reuse the helper of
	// notify_test.go (newWebhookRecorder or equivalent — read it first).
	f := newAzureFake(t, "site1")
	f.costs = map[string]float64{"site1": 95}
	defer f.srv.Close()
	h := newTestHub(t, f)
	rec := newWebhookRecorder(t)
	defer rec.Close()
	_ = h.st.SetSetting("azure_budget_monthly", "100")
	_ = h.st.SetSetting("webhook_url", rec.URL)
	_ = h.st.SetSetting("webhook_enabled", "1")
	h.ReloadNotify()
	h.ReloadAzure()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.dispatch.Start(ctx)
	h.syncAzureCosts(ctx)
	got := rec.wait(t, 2, 5*time.Second) // 80 % and 100 % thresholds... 95 fires only 80
	if !strings.Contains(got[0], "Budget") || !strings.Contains(got[0], "80") {
		t.Fatalf("message = %q", got[0])
	}
	// Every delivery row points at the guardrail journal.
	ds, _ := h.st.ListDeliveries(10)
	for _, d := range ds {
		if d.GuardrailEventID == nil {
			t.Fatalf("delivery %d has no guardrail event", d.ID)
		}
	}
}
```

Adjust the expected count in the third test to what 95 % actually fires with the default thresholds (only `80`): `rec.wait(t, 1, …)`.

- [ ] **Step 4: Run to see them fail**

Run: `go test ./internal/hub/ -run 'Guardrail|FailedSyncEvaluates' -v`
Expected: FAIL — no orphan event, no share event, no delivery.

- [ ] **Step 5: Implement the hub wiring**

```go
// internal/hub/guardrails_hub.go
package hub

import (
	"context"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func (h *Hub) GuardrailSettings() guardrails.Settings { return guardrails.LoadSettings(h.st) }

// evaluateBudget runs after a successful cost sync and journals only what
// changed. It reads the same rows the page reads, so the two never disagree.
func (h *Hub) evaluateBudget() {
	cfg := azure.LoadConfig(h.st)
	now := time.Now().UTC()
	costs, err := h.st.ListAzureCosts(currentPeriod(now))
	if err != nil {
		h.log.Error("guardrails: list costs", "err", err)
		return
	}
	names := h.resourceNames()
	var in guardrails.BudgetInput
	in.Now, in.Budget, in.Settings = now, cfg.BudgetMonthly, h.GuardrailSettings()
	currencies := map[string]bool{}
	for _, c := range costs {
		in.Spent += c.Amount
		currencies[c.Currency] = true
		if c.AsOf.After(in.AsOf) {
			in.AsOf = c.AsOf
		}
		in.Rows = append(in.Rows, guardrails.CostRow{ResourceID: c.ResourceID, Name: names[c.ResourceID], Amount: c.Amount})
	}
	in.Currencies = len(currencies)
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	in.Last = last
	h.journalGuardrails(guardrails.Budget(in))
}

// evaluateOrphans runs inside a successful inventory sync, with the freshly
// read resources: the typed reads happen before the rows are stored, so a
// read that fails fails the whole sync (a partial inventory is worse than none).
func (h *Hub) evaluateOrphans(ctx context.Context, client *azure.Client, rs []azure.Resource) (map[string]string, error) {
	reasons, err := azure.OrphanReasons(ctx, client, rs)
	if err != nil {
		return nil, err
	}
	set := h.GuardrailSettings()
	now := time.Now().UTC()
	for _, r := range rs {
		if r.Tags[azure.CreatedByTag] != azure.CreatedByValue {
			continue
		}
		hostID, ok := parseHostID(r.Tags[azure.HostIDTag])
		if !ok {
			continue
		}
		host, err := h.st.Host(hostID)
		if err != nil {
			continue
		}
		if guardrails.HubVMSilent(host.Status, host.LastSeen, now, set.HubVMSilentDays) {
			reasons[r.ID] = azure.OrphanHubVMSilent
		}
	}
	return reasons, nil
}

func (h *Hub) journalOrphans(reasons map[string]string) {
	now := time.Now().UTC()
	if err := h.st.SetAzureOrphans(reasons, now); err != nil {
		h.log.Error("guardrails: store orphans", "err", err)
		return
	}
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	h.journalGuardrails(guardrails.Orphans(guardrails.OrphanInput{Now: now, Reasons: reasons, Names: h.resourceNames(), Last: last}))
}

// journalGuardrails is the single writer of the guardrail journal. Every line
// it writes is published and handed to the dispatcher, success and failure
// alike (lot 3's silent failure is the defect this guards against).
func (h *Hub) journalGuardrails(evs []store.GuardrailEvent) {
	var saved []store.GuardrailEvent
	for _, e := range evs {
		stored, err := h.st.InsertGuardrailEvent(e)
		if err != nil {
			h.log.Error("guardrails: insert", "err", err)
			continue
		}
		h.log.Info("guardrail", "rule", e.Rule, "subject", e.Subject, "kind", e.Kind, "value", e.Value)
		saved = append(saved, stored)
	}
	if len(saved) > 0 {
		h.notifyGuardrails(saved)
		h.bus.Publish("azure", map[string]any{"scope": "guardrails", "ok": true})
	}
}

// journalScheduleFailure fires schedule_failed for a boundary that produced no
// action row. Kept beside runSchedules because that is the only caller; the
// resolve side lives in finishAction, where a later success is observed.
func (h *Hub) journalScheduleFailure(resourceID, msg string) {
	last, err := h.st.LastGuardrailEventPerKey()
	if err != nil {
		h.log.Error("guardrails: last events", "err", err)
		return
	}
	if last[store.GuardrailKey{Subject: resourceID, Rule: "schedule_failed"}].Kind == "fired" {
		return
	}
	h.log.Warn("schedules: boundary not applied", "resource", resourceID, "err", msg)
	h.journalGuardrails([]store.GuardrailEvent{{Subject: resourceID, Rule: "schedule_failed", Kind: "fired", At: time.Now().UTC()}})
}

func (h *Hub) resourceNames() map[string]string {
	out := map[string]string{}
	rs, err := h.st.ListAzureResources()
	if err != nil {
		return out
	}
	for _, r := range rs {
		out[r.ID] = r.Name
	}
	return out
}

func parseHostID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}
```
(add `"strconv"` to the imports.)

In `azure_sync.go`:
- `syncAzureInventory`: leave the existing body untouched through `recordAzureSync("inventory", true, …)`. Then, and only then:

```go
	// The orphan sweep is its own scope. It runs after the inventory is
	// stored, because it needs nothing from the store, and a read that fails
	// must not make the inventory look stale: the catalogue and enrichment
	// passes succeeded, and the page's banner would be lying.
	started = time.Now().UTC()
	reasons, err := h.evaluateOrphans(ctx, client, rs)
	if err != nil {
		h.log.Warn("azure orphan sweep failed", "err", err)
		h.recordAzureSync("orphans", false, err.Error(), started)
		return
	}
	h.recordAzureSync("orphans", true, "", started)
	h.journalOrphans(reasons)
```
- `syncAzureCosts`: after `recordAzureSync("cost", true, …)`, call `h.evaluateBudget()`.

In `notify.go`, add:

```go
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
		m := guardrailMessage(cfg, e, names[e.Subject])
		for _, ch := range chans {
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
		subject = azure.LastSegment(e.Subject) // package hub has no lastSegment; the copy lives in server
	}
	metric := e.Rule
	if e.Detail != "" {
		metric = e.Rule + " " + e.Detail + "%"
	}
	return notify.Message{HostName: subject, Metric: metric, Kind: e.Kind, Value: e.Value, At: e.At, Link: cfg.AzureLink()}
}
```

`cfg.Link(hostID)` exists in `notify.Config`; add `AzureLink()` beside it, returning `<public URL>/azure` or `""` when no public URL is configured (read `notify/config.go` for the field name). `lastSegment` exists in `server/api_azure.go`; move a copy into `internal/hub/azure` as `LastSegment` and use it from both places rather than duplicating. In `replayPendingDeliveries`, when `h.st.DeliveryEvent(d.ID)` returns `store.ErrNotFound`, try `h.st.DeliveryGuardrailEvent(d.ID)` and build the message with `guardrailMessage`; only when both fail, `continue`.

In `notify/message.go`, `ValueText` switches on `Metric`: add a branch so that a metric starting with `budget_` or equal to `resource_share` renders `%.0f %%` for share/thresholds and money-less figures otherwise; `orphan` renders nothing; `schedule_failed` renders nothing. Keep it small; the channels only need a readable line.

In `server/api_notify.go` (deliveries list): when `DeliveryEvent` fails with `ErrNotFound`, call `DeliveryGuardrailEvent` and fill `v.Host` with the subject (name looked up through `Store.ListAzureResources` once per request, or the last segment), `v.Metric` with the rule, `v.Kind` with the kind.

- [ ] **Step 6: Run the hub tests**

Run: `go test ./internal/hub/... -race`
Expected: PASS.

- [ ] **Step 7: Mutation check**

In `syncAzureCosts`, call `h.evaluateBudget()` **before** the `if err != nil` return of the cost query; `TestAFailedSyncEvaluatesNothing` fails. Restore.

- [ ] **Step 8: Commit**

```bash
git add internal/hub/guardrails/orphans.go internal/hub/guardrails/orphans_test.go internal/hub/guardrails_hub.go internal/hub/guardrails_hub_test.go internal/hub/azure_sync.go internal/hub/notify.go internal/hub/notify/ internal/hub/azure/ internal/hub/server/api_notify.go internal/hub/azure_test.go
git commit -m "feat(hub): evaluate budget and orphans after each successful sync, deliver through lot 2"
```

---

### Task 7: Schedule windows (pure)

**Files:**
- Create: `internal/hub/guardrails/schedule.go`, `internal/hub/guardrails/schedule_test.go`

**Interfaces:**
- Produces:
  ```go
  type Window struct {
      Days []int  `json:"days"` // ISO: 1 = Monday … 7 = Sunday
      From string `json:"from"` // "HH:MM"
      To   string `json:"to"`   // "HH:MM"; To <= From means the window crosses midnight
  }
  func ParseWindows(raw string) ([]Window, error)   // validates, merges, sorts; "" or "[]" → empty
  func EncodeWindows(ws []Window) string
  // Off reports whether t (any zone) falls inside an off window, evaluated in loc.
  func Off(ws []Window, loc *time.Location, t time.Time) bool
  // LastBoundary returns the most recent boundary at or before t, and whether
  // the resource should be off after it. ok=false when there is no window.
  func LastBoundary(ws []Window, loc *time.Location, t time.Time) (at time.Time, off bool, ok bool)
  var EveningsAndWeekends = []Window{{Days: []int{1,2,3,4,5}, From: "20:00", To: "07:00"}, {Days: []int{6,7}, From: "00:00", To: "24:00"}}
  ```
  A window `{days, from, to}` means: on each listed day, the resource is off from `from`; if `to <= from` the window ends at `to` on the **next** day. `"24:00"` is accepted as `to` only, meaning end of day. Merging: overlapping or touching intervals on the same calendar day are joined at evaluation time by working on absolute intervals; `ParseWindows` only validates and sorts (merging JSON windows is not needed for correctness, and a window list a person wrote should read back as written).

- [ ] **Step 1: Write the failing tests**

```go
// internal/hub/guardrails/schedule_test.go
package guardrails

import (
	"testing"
	"time"
)

func paris(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no zone database")
	}
	return loc
}

func at(loc *time.Location, y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

func TestParseWindowsValidates(t *testing.T) {
	if ws, err := ParseWindows(`[{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]`); err != nil || len(ws) != 1 {
		t.Fatalf("valid: %v %v", ws, err)
	}
	for _, bad := range []string{`[{"days":[0],"from":"20:00","to":"07:00"}]`, `[{"days":[1],"from":"25:00","to":"07:00"}]`, `[{"days":[1],"from":"20:00","to":"7"}]`, `[{"days":[],"from":"20:00","to":"07:00"}]`, `{"days":[1]}`} {
		if _, err := ParseWindows(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if ws, err := ParseWindows(""); err != nil || len(ws) != 0 {
		t.Fatalf("empty: %v %v", ws, err)
	}
}

func TestOffAcrossMidnightAndWeekend(t *testing.T) {
	loc := paris(t)
	ws := EveningsAndWeekends
	// Wednesday 2026-09-23.
	if Off(ws, loc, at(loc, 2026, 9, 23, 12, 0)) {
		t.Fatal("Wednesday noon is on")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 23, 21, 0)) {
		t.Fatal("Wednesday 21:00 is off")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 24, 6, 30)) {
		t.Fatal("Thursday 06:30 is still off (window crossed midnight)")
	}
	if Off(ws, loc, at(loc, 2026, 9, 24, 7, 0)) {
		t.Fatal("Thursday 07:00 is on")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 26, 12, 0)) || !Off(ws, loc, at(loc, 2026, 9, 27, 23, 59)) {
		t.Fatal("weekend is off")
	}
	// The Friday evening window runs into Saturday, which is off anyway; Monday 06:59 is off (Sunday→Monday? no: Sunday is 00:00–24:00, Monday 00:00 is on).
	if Off(ws, loc, at(loc, 2026, 9, 28, 0, 30)) {
		t.Fatal("Monday 00:30 is on: no weekday window started on Sunday")
	}
	// Given in UTC, evaluated in Paris: 19:00 UTC on a Wednesday in September = 21:00 Paris → off.
	if !Off(ws, loc, time.Date(2026, 9, 23, 19, 0, 0, 0, time.UTC)) {
		t.Fatal("zone must be applied")
	}
}

func TestLastBoundary(t *testing.T) {
	loc := paris(t)
	ws := EveningsAndWeekends
	b, off, ok := LastBoundary(ws, loc, at(loc, 2026, 9, 23, 21, 30))
	if !ok || !off || !b.Equal(at(loc, 2026, 9, 23, 20, 0)) {
		t.Fatalf("Wed 21:30 → last boundary Wed 20:00 (off): %v %v %v", b, off, ok)
	}
	b, off, _ = LastBoundary(ws, loc, at(loc, 2026, 9, 24, 8, 0))
	if off || !b.Equal(at(loc, 2026, 9, 24, 7, 0)) {
		t.Fatalf("Thu 08:00 → Thu 07:00 (on): %v %v", b, off)
	}
	b, off, _ = LastBoundary(ws, loc, at(loc, 2026, 9, 28, 3, 0))
	if off || !b.Equal(at(loc, 2026, 9, 28, 0, 0)) {
		t.Fatalf("Mon 03:00 → Mon 00:00 (on, weekend ended): %v %v", b, off)
	}
	if _, _, ok := LastBoundary(nil, loc, at(loc, 2026, 9, 28, 3, 0)); ok {
		t.Fatal("no window, no boundary")
	}
}

func TestDaylightSaving(t *testing.T) {
	loc := paris(t)
	// Spring 2026: clocks jump from 02:00 to 03:00 on Sunday 2026-03-29.
	ws := []Window{{Days: []int{7}, From: "02:30", To: "04:00"}}
	b, off, ok := LastBoundary(ws, loc, at(loc, 2026, 3, 29, 3, 15))
	if !ok || !off || b.Hour() != 3 || b.Minute() != 0 {
		t.Fatalf("a boundary in the skipped hour moves to 03:00: %v %v", b, off)
	}
	// Autumn 2026: 03:00 falls back to 02:00 on Sunday 2026-10-25; 02:30 happens twice.
	ws = []Window{{Days: []int{7}, From: "02:30", To: "05:00"}}
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(loc)  // 02:30 CEST, first occurrence
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(loc) // 02:30 CET, second occurrence
	b1, _, _ := LastBoundary(ws, loc, first.Add(5*time.Minute))
	b2, _, _ := LastBoundary(ws, loc, second.Add(5*time.Minute))
	if !b1.Equal(b2) {
		t.Fatalf("the repeated hour must yield one boundary, got %v and %v", b1, b2)
	}
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/guardrails/ -run 'Windows|Off|LastBoundary|Daylight' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

```go
// internal/hub/guardrails/schedule.go
package guardrails

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Window struct {
	Days []int  `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

var EveningsAndWeekends = []Window{
	{Days: []int{1, 2, 3, 4, 5}, From: "20:00", To: "07:00"},
	{Days: []int{6, 7}, From: "00:00", To: "24:00"},
}

func ParseWindows(raw string) ([]Window, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ws []Window
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		return nil, fmt.Errorf("windows must be a JSON list: %w", err)
	}
	for i, w := range ws {
		if len(w.Days) == 0 {
			return nil, fmt.Errorf("window %d has no day", i+1)
		}
		for _, d := range w.Days {
			if d < 1 || d > 7 {
				return nil, fmt.Errorf("window %d: day %d is not between 1 (Monday) and 7 (Sunday)", i+1, d)
			}
		}
		if _, err := minutes(w.From, false); err != nil {
			return nil, fmt.Errorf("window %d from: %w", i+1, err)
		}
		if _, err := minutes(w.To, true); err != nil {
			return nil, fmt.Errorf("window %d to: %w", i+1, err)
		}
		sort.Ints(ws[i].Days)
	}
	return ws, nil
}

func EncodeWindows(ws []Window) string {
	if len(ws) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

// minutes parses "HH:MM" into minutes since midnight. "24:00" is allowed as
// an end only.
func minutes(s string, end bool) (int, error) {
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%2d:%2d", &hh, &mm); err != nil || len(s) != 5 {
		return 0, errors.New("expected HH:MM")
	}
	if mm < 0 || mm > 59 || hh < 0 || hh > 24 || (hh == 24 && (mm != 0 || !end)) {
		return 0, fmt.Errorf("%q is not a time of day", s)
	}
	return hh*60 + mm, nil
}

type interval struct{ start, end time.Time }

// intervals lists every off interval that starts on a calendar day within
// [t-2d, t+1d] in loc. Two days back covers a window that started the day
// before and crosses midnight; the DST rules live in time.Date.
func intervals(ws []Window, loc *time.Location, t time.Time) []interval {
	var out []interval
	tl := t.In(loc)
	for dayOff := -2; dayOff <= 1; dayOff++ {
		day := time.Date(tl.Year(), tl.Month(), tl.Day()+dayOff, 0, 0, 0, 0, loc)
		iso := int(day.Weekday())
		if iso == 0 {
			iso = 7
		}
		for _, w := range ws {
			if !contains(w.Days, iso) {
				continue
			}
			from, _ := minutes(w.From, false)
			to, _ := minutes(w.To, true)
			start := clock(day, from)
			var end time.Time
			if to <= from {
				end = clock(time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc), to)
			} else {
				end = clock(day, to)
			}
			out = append(out, interval{start, end})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return merge(out)
}

// clock is time.Date at a wall-clock minute of the day. In the spring gap
// time.Date moves 02:30 to 03:30; the spec wants the next hour, so a start
// that lands in the gap is pushed to the first instant after it. In the
// autumn overlap time.Date picks the first occurrence, which is the one
// boundary the spec wants applied.
func clock(day time.Time, mins int) time.Time {
	t := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).Add(time.Duration(mins) * time.Minute)
	wall := time.Date(day.Year(), day.Month(), day.Day(), mins/60, mins%60, 0, 0, day.Location())
	if wall.Hour()*60+wall.Minute() != mins%(24*60) && mins < 24*60 {
		// the wall clock did not exist that day: the gap; use the first
		// instant after the transition
		return wall.Truncate(time.Hour)
	}
	if mins == 24*60 {
		return t
	}
	return wall
}

func merge(in []interval) []interval {
	var out []interval
	for _, iv := range in {
		if n := len(out); n > 0 && !iv.start.After(out[n-1].end) {
			if iv.end.After(out[n-1].end) {
				out[n-1].end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func Off(ws []Window, loc *time.Location, t time.Time) bool {
	for _, iv := range intervals(ws, loc, t) {
		if !t.Before(iv.start) && t.Before(iv.end) {
			return true
		}
	}
	return false
}

func LastBoundary(ws []Window, loc *time.Location, t time.Time) (time.Time, bool, bool) {
	var best time.Time
	off, ok := false, false
	for _, iv := range intervals(ws, loc, t) {
		if !iv.start.After(t) && iv.start.After(best) {
			best, off, ok = iv.start, true, true
		}
		if !iv.end.After(t) && iv.end.After(best) {
			best, off, ok = iv.end, false, true
		}
	}
	return best, off, ok
}
```

**On `clock` and daylight saving.** `time.Date` normalises a wall clock that does not exist by
pushing it forward by the gap, so 02:30 on the spring day becomes 03:30, not the 03:00 the spec
asks for; and it picks the first occurrence of a repeated wall clock, which is the single boundary
the spec wants in autumn. Only the spring case needs correcting, and `Truncate` is the wrong tool
(it rounds in absolute time and lands on a wall-clock hour only in whole-hour offset zones). Detect
the gap by comparing the wall clock back out, and snap to the instant the transition ends:

```go
// clock is a wall-clock minute of a given day, in that day's zone.
//
// Two daylight-saving cases. Spring: the wall clock 02:30 does not exist, and
// time.Date answers 03:30 — one hour past where the spec wants the boundary.
// We detect that (the minute we asked for is not the minute we got) and snap
// to the first instant of the new offset, 03:00. Autumn: 02:30 happens twice,
// and time.Date returns the first occurrence, which is the one boundary the
// spec wants applied.
func clock(day time.Time, mins int) time.Time {
	if mins >= 24*60 { // "24:00" is the end of the day, i.e. midnight next day
		return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location())
	}
	t := time.Date(day.Year(), day.Month(), day.Day(), mins/60, mins%60, 0, 0, day.Location())
	if t.Hour()*60+t.Minute() == mins {
		return t
	}
	// The wall clock was skipped. Walk back to the last existing minute before
	// the gap, then forward one minute: that is the first instant of the new
	// offset, whatever the size of the jump.
	before := time.Date(day.Year(), day.Month(), day.Day(), mins/60, mins%60, 0, 0, day.Location()).Add(-time.Duration(t.Hour()*60+t.Minute()-mins) * time.Minute)
	return before.Add(time.Minute).Truncate(time.Minute)
}
```

Run the DST test against this and read what it prints before changing anything else; if the simpler
form (`t` when the minute matches, otherwise the start of the following hour in that zone) passes
both assertions, keep the simpler form. Never weaken the test to make it pass.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/guardrails/ -race -v`
Expected: PASS.

- [ ] **Step 5: Mutation check**

In `intervals`, change `if to <= from` to `if to < from`; `TestOffAcrossMidnightAndWeekend` (the `00:00`–`24:00` weekend window is unaffected, but a window `20:00`–`20:00` would be) — instead mutate `merge` to never merge and `TestLastBoundary`'s Monday case must still pass; so the meaningful mutation is in `LastBoundary`: drop the `iv.end` branch; `TestLastBoundary` fails on "Thu 08:00 → Thu 07:00". Do that one. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/guardrails/schedule.go internal/hub/guardrails/schedule_test.go
git commit -m "feat(guardrails): off windows — parse, evaluate, last boundary, daylight saving"
```

---

### Task 8: Schedules in the hub — boundary crossing, catch-up, failure event

**Symbols landed earlier with no caller — this task wires them.** Tasks 5 and 6
deliberately shipped two symbols that nothing calls yet, and two separate reviews
flagged each as unverifiable from its own diff. Both belong to this task. Confirm
each is genuinely wired here, and say so in the report so the review does not have
to rediscover the gap:

- `azure.OrphanHubVMSilent` (`internal/hub/azure/orphans.go`) — the reason string
  for a hub-provisioned VM whose agent has gone quiet.
- `(*Hub).journalScheduleFailure` (`internal/hub/guardrails_hub.go`) — fires and
  resolves the `schedule_failed` rule. Note that it must be called from **two**
  places, not one: `finishAction` when a scheduled action fails after its row
  exists, and `runSchedules` when no action row is ever created at all (the
  resource left the inventory, or its type stopped being actionable). The second
  is the failure most likely to happen in practice and the easiest to forget.

**Files:**
- Modify: `internal/hub/guardrails_hub.go` (add `applySchedules`, `catchUpSchedules`)
- Modify: `internal/hub/hub.go` (minute tick calls `applySchedules`; `Run` calls `catchUpSchedules` once after `interruptActions`)
- Modify: `internal/hub/azure_action.go` (`StartAction` gains an origin; `finishAction` journals `schedule_failed` / resolves it)
- Modify: `internal/hub/server/api_azure.go` (`Azurer.StartAction` signature unchanged for the API: the server keeps calling `StartAction(id, action)`; add `StartActionFrom(id, action, origin)` on the hub and have `StartAction` call it with `"user"`)
- Test: `internal/hub/guardrails_hub_test.go`

**Interfaces:**
- Produces:
  ```go
  func (h *Hub) StartActionFrom(resourceID string, a azure.Action, origin string) (int64, error)
  func (h *Hub) applySchedules(now time.Time)   // called every minute
  func (h *Hub) catchUpSchedules(now time.Time) // called once at startup; applies a missed boundary < 12h old
  const catchUpWindow = 12 * time.Hour
  ```

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/hub/guardrails_hub_test.go

func scheduleOn(t *testing.T, h *Hub, id string) {
	t.Helper()
	if err := h.st.UpsertAzureSchedule(store.AzureSchedule{ResourceID: id, OffWindows: guardrails.EncodeWindows(guardrails.EveningsAndWeekends), Enabled: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestABoundaryIssuesExactlyOneScheduledAction(t *testing.T) {
	f := newAzureFake(t, "site1")
	defer f.srv.Close()
	h := newTestHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))

	wed2030 := time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC)
	h.applySchedules(wed2030)
	f.waitActions(t, 1) // helper: waits until the fake saw n action posts
	as, _ := h.st.ListAzureActions(5)
	if len(as) != 1 || as[0].Action != "stop" || as[0].Origin != "schedule" {
		t.Fatalf("actions = %+v", as)
	}
	// Ten minutes later: same boundary, nothing new.
	h.applySchedules(wed2030.Add(10 * time.Minute))
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 {
		t.Fatalf("replayed: %+v", as)
	}
	// Next morning: the end boundary → start.
	h.applySchedules(time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC))
	f.waitActions(t, 2)
	as, _ = h.st.ListAzureActions(5)
	if as[0].Action != "start" {
		t.Fatalf("morning action = %+v", as[0])
	}
}

func TestCatchUpIsBoundedToTwelveHours(t *testing.T) {
	// With evenings-and-weekends in UTC, the Friday 20:00 window merges with
	// the Saturday and Sunday all-day windows into one off interval running to
	// Monday 00:00. So the last boundary anywhere in the weekend is
	// Friday 2026-09-25 20:00 UTC, and this pair also exercises the merge.
	for _, tc := range []struct {
		name    string
		wake    time.Time
		applied bool
	}{
		{"11h59 after the boundary", time.Date(2026, 9, 26, 7, 59, 0, 0, time.UTC), true},
		{"12h01 after the boundary", time.Date(2026, 9, 26, 8, 1, 0, 0, time.UTC), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFake(t, "site1")
			defer f.srv.Close()
			h := newTestHub(t, f)
			_ = h.st.SetSetting("azure_timezone", "UTC")
			h.ReloadAzure()
			h.syncAzureInventory(context.Background())
			scheduleOn(t, h, f.id("site1"))

			h.catchUpSchedules(tc.wake)
			if tc.applied {
				f.waitActions(t, 1)
				as, _ := h.st.ListAzureActions(5)
				if len(as) != 1 || as[0].Action != "stop" || as[0].Origin != "schedule" {
					t.Fatalf("expected one scheduled stop, got %+v", as)
				}
			} else {
				time.Sleep(100 * time.Millisecond)
				if as, _ := h.st.ListAzureActions(5); len(as) != 0 {
					t.Fatalf("a boundary older than twelve hours must not be applied: %+v", as)
				}
			}
			// Either way the boundary is marked, so the next tick does not
			// reconsider it.
			scs, _ := h.st.ListAzureSchedules()
			if scs[0].LastBoundary == nil || !scs[0].LastBoundary.Equal(time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)) {
				t.Fatalf("boundary not marked: %+v", scs[0].LastBoundary)
			}
		})
	}
}

func TestABoundaryThatProducesNoActionRowStillFires(t *testing.T) {
	f := newAzureFake(t, "site1")
	defer f.srv.Close()
	h := newTestHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	// A schedule on a resource the inventory does not have: StartActionFrom
	// refuses before writing any row, so finishAction never runs.
	if err := h.st.UpsertAzureSchedule(store.AzureSchedule{ResourceID: "/s/gone", OffWindows: guardrails.EncodeWindows(guardrails.EveningsAndWeekends), Enabled: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	waitFor(t, 5*time.Second, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: "/s/gone", Rule: "schedule_failed"}].Kind == "fired"
	})
	// It fires once, not every minute.
	h.applySchedules(time.Date(2026, 9, 23, 20, 31, 0, 0, time.UTC))
	time.Sleep(100 * time.Millisecond)
	n := 0
	evs, _ := h.st.ListGuardrailEvents(20)
	for _, e := range evs {
		if e.Rule == "schedule_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("schedule_failed journaled %d times", n)
	}
}
```

```go
func TestAFailedScheduledActionFiresScheduleFailed(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.actionStatus = 500
	defer f.srv.Close()
	h := newTestHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	waitFor(t, 5*time.Second, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: f.id("site1"), Rule: "schedule_failed"}].Kind == "fired"
	})
	// The next boundary succeeds and resolves it.
	f.actionStatus = 0
	h.applySchedules(time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC))
	waitFor(t, 5*time.Second, func() bool {
		last, _ := h.st.LastGuardrailEventPerKey()
		return last[store.GuardrailKey{Subject: f.id("site1"), Rule: "schedule_failed"}].Kind == "resolved"
	})
}

func TestAManualActionInFlightSkipsTheBoundary(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.holdAction = make(chan struct{})
	defer f.srv.Close()
	h := newTestHub(t, f)
	_ = h.st.SetSetting("azure_timezone", "UTC")
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	scheduleOn(t, h, f.id("site1"))
	if _, err := h.StartAction(f.id("site1"), azure.ActionRestart); err != nil {
		t.Fatal(err)
	}
	h.applySchedules(time.Date(2026, 9, 23, 20, 30, 0, 0, time.UTC))
	close(f.holdAction)
	f.waitActions(t, 1)
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 || as[0].Origin != "user" {
		t.Fatalf("the boundary must be skipped, not queued: %+v", as)
	}
	// And it is not retried on the next tick either: last_boundary advanced.
	h.applySchedules(time.Date(2026, 9, 23, 20, 31, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if as, _ := h.st.ListAzureActions(5); len(as) != 1 {
		t.Fatalf("retried: %+v", as)
	}
}
```

`waitFor` exists in `internal/hub/ingest` tests; copy the three-line helper into `guardrails_hub_test.go` if the hub package has none.

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/ -run 'Boundary|CatchUp|ScheduleFailed|InFlightSkips' -v`
Expected: FAIL — undefined `applySchedules`, etc.

- [ ] **Step 3: Implement**

In `azure_action.go`:

```go
func (h *Hub) StartAction(resourceID string, a azure.Action) (int64, error) {
	return h.StartActionFrom(resourceID, a, "user")
}

// StartActionFrom is StartAction with an origin. Schedules pass "schedule";
// the row says so, and the failure of a scheduled action becomes a guardrail
// event rather than a line only the action log knows.
func (h *Hub) StartActionFrom(resourceID string, a azure.Action, origin string) (int64, error) {
	// … existing body, with Origin: origin in the store.AzureAction literal,
	// and `go h.runAction(actionID, r, a, client, origin)`
}
```

Thread `origin` through `runAction` and `finishAction(actionID, status, errMsg, state, r, origin)`; at the end of `finishAction`, when `origin == "schedule"`:

```go
	if origin == "schedule" {
		// journalScheduleFailure in guardrails_hub.go does the same for a
		// boundary that never produced an action row at all.
		last, err := h.st.LastGuardrailEventPerKey()
		if err != nil {
			h.log.Error("guardrails: last events", "err", err)
			return
		}
		key := store.GuardrailKey{Subject: r.ID, Rule: "schedule_failed"}
		firing := last[key].Kind == "fired"
		switch {
		case status == "failed" && !firing:
			h.journalGuardrails([]store.GuardrailEvent{{Subject: r.ID, Rule: "schedule_failed", Kind: "fired", At: time.Now().UTC()}})
		case status == "succeeded" && firing:
			h.journalGuardrails([]store.GuardrailEvent{{Subject: r.ID, Rule: "schedule_failed", Kind: "resolved", At: time.Now().UTC()}})
		}
	}
```

In `guardrails_hub.go`:

```go
const catchUpWindow = 12 * time.Hour

// applySchedules acts on boundaries, never on states: a resource switched on
// by hand inside its window stays on until the next boundary.
func (h *Hub) applySchedules(now time.Time) { h.runSchedules(now, 0) }

// catchUpSchedules is applySchedules with an age limit, for the boundaries
// the hub slept through.
func (h *Hub) catchUpSchedules(now time.Time) { h.runSchedules(now, catchUpWindow) }

func (h *Hub) runSchedules(now time.Time, maxAge time.Duration) {
	if _, _, ok := h.azureReady(); !ok {
		return
	}
	scs, err := h.st.ListAzureSchedules()
	if err != nil {
		h.log.Error("schedules: list", "err", err)
		return
	}
	loc := h.GuardrailSettings().Location()
	for _, sc := range scs {
		if !sc.Enabled {
			continue
		}
		ws, err := guardrails.ParseWindows(sc.OffWindows)
		if err != nil || len(ws) == 0 {
			continue
		}
		b, off, ok := guardrails.LastBoundary(ws, loc, now)
		if !ok || (sc.LastBoundary != nil && !b.After(*sc.LastBoundary)) {
			continue
		}
		if maxAge > 0 && now.Sub(b) > maxAge {
			h.log.Warn("schedules: missed boundary, too old to apply", "resource", sc.ResourceID, "boundary", b, "age", now.Sub(b))
			if err := h.st.MarkScheduleBoundary(sc.ResourceID, b, now); err != nil {
				h.log.Error("schedules: mark boundary", "err", err)
			}
			continue
		}
		// Advance first: a crash after this line loses the action, never
		// duplicates it (lot 4's rule for interrupted actions).
		if err := h.st.MarkScheduleBoundary(sc.ResourceID, b, now); err != nil {
			h.log.Error("schedules: mark boundary", "err", err)
			continue
		}
		action := azure.ActionStart
		if off {
			action = azure.ActionStop
		}
		if _, err := h.StartActionFrom(sc.ResourceID, action, "schedule"); err != nil {
			if errors.Is(err, store.ErrActionInFlight) {
				h.log.Warn("schedules: a manual action is running, boundary skipped", "resource", sc.ResourceID, "boundary", b)
				continue
			}
			// No action row exists, so finishAction will never run and would
			// never journal this. A resource that left the inventory, or whose
			// type stopped being actionable, would otherwise stay on all night
			// with nothing said. Spec §5 promises an event here.
			h.log.Warn("schedules: action refused", "resource", sc.ResourceID, "action", action, "err", err)
			h.journalScheduleFailure(sc.ResourceID, err.Error())
		}
	}
}
```

Read `StartAzureAction` in the store to confirm which error an in-flight row produces (`ErrActionInFlight`) and match it.

In `hub.go`: in the `minute.C` case, add `h.applySchedules(time.Now().UTC())` after `h.evaluate()`; after `h.interruptProvisions()` in `Run`, add `h.catchUpSchedules(time.Now().UTC())`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/... -race`
Expected: PASS.

- [ ] **Step 5: Mutation check**

Move `MarkScheduleBoundary` after `StartActionFrom`; `TestAManualActionInFlightSkipsTheBoundary` fails on "retried". Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/
git commit -m "feat(hub): stop schedules act on boundary crossings, catch up within twelve hours, report failures"
```

---

### Task 9: Orphan deletion by name

**Files:**
- Modify: `internal/hub/azure/delete.go` (`apiVersionFor` learns `disk`, `plan`; new `DeleteAny`)
- Modify: `internal/hub/azure/delete_test.go`
- Modify: `internal/hub/guardrails_hub.go` (`DeleteOrphan`)
- Test: `internal/hub/guardrails_hub_test.go`

**Interfaces:**
- Produces:
  ```go
  // azure
  var ErrUndeletableKind = errors.New("azure: the hub's role cannot delete this kind of resource")
  func KindOf(resourceType string) (string, bool) // "Microsoft.Compute/disks" → "disk"; NICs → "nic"; serverfarms → "plan"; VMs → "vm"; publicIPAddresses → "ip" (deletable=false)
  // DeleteAny deletes one resource of a deletable kind, with no ownership check:
  // the person typed its name. A 404 counts as deleted.
  func DeleteAny(ctx context.Context, c *Client, armID, kind string) error
  // hub
  func (h *Hub) DeleteOrphan(resourceID, confirmName string) error
  ```

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/hub/azure/delete_test.go
func TestKindOfAndWhatTheRoleCanDelete(t *testing.T) {
	cases := map[string][2]any{
		"Microsoft.Compute/disks":             {"disk", true},
		"Microsoft.Network/networkInterfaces": {"nic", true},
		"Microsoft.Web/serverfarms":           {"plan", true},
		"Microsoft.Compute/virtualMachines":   {"vm", true},
		"Microsoft.Network/publicIPAddresses": {"ip", false},
		"Microsoft.Web/sites":                 {"", false},
	}
	for typ, want := range cases {
		kind, ok := KindOf(typ)
		if kind != want[0].(string) || ok != want[1].(bool) {
			t.Errorf("%s → %q %v, want %q %v", typ, kind, ok, want[0], want[1])
		}
	}
}

func TestDeleteAnyDoesNotCheckOwnershipAndTreats404AsDone(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		if r.Method == http.MethodGet {
			t.Errorf("no GET expected (no ownership check), got %s", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/gone") {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"code":"ResourceNotFound","message":"gone"}}`)
				return
			}
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if err := DeleteAny(context.Background(), c, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "disk"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAny(context.Background(), c, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/gone", "disk"); err != nil {
		t.Fatalf("404 is done: %v", err)
	}
	if err := DeleteAny(context.Background(), c, "/x/ip1", "ip"); !errors.Is(err, ErrUndeletableKind) {
		t.Fatalf("ip must be refused before any call: %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deletes = %v", deleted)
	}
}
```

```go
// append to internal/hub/guardrails_hub_test.go
func TestDeleteOrphanNeedsTheNameAndADeletableKind(t *testing.T) {
	f := newAzureFake(t, "site1")
	f.disks = []string{"d1"}
	f.ips = []string{"ip1"}
	defer f.srv.Close()
	h := newTestHub(t, f)
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	if err := h.DeleteOrphan(f.id("d1"), "d2"); !errors.Is(err, ErrWrongName) {
		t.Fatalf("wrong name: %v", err)
	}
	if err := h.DeleteOrphan(f.id("ip1"), "ip1"); !errors.Is(err, azure.ErrUndeletableKind) {
		t.Fatalf("ip: %v", err)
	}
	if err := h.DeleteOrphan(f.id("d1"), "d1"); err != nil {
		t.Fatal(err)
	}
	f.waitDeletes(t, 1)
	// The next sweep no longer lists it (the fake dropped it), and the orphan event resolves.
	f.disks = nil
	h.syncAzureInventory(context.Background())
	last, _ := h.st.LastGuardrailEventPerKey()
	if last[store.GuardrailKey{Subject: f.id("d1"), Rule: "orphan"}].Kind != "resolved" {
		t.Fatalf("orphan not resolved after deletion: %v", last)
	}
}
```

Extend the fake: `ips []string` listed as `Microsoft.Network/publicIPAddresses` (answering `GET` with no `ipConfiguration`), `DELETE` handling that records and removes from `disks`, `waitDeletes(t, n)`.

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/azure/ ./internal/hub/ -run 'KindOf|DeleteAny|DeleteOrphan' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

In `delete.go`:

```go
var ErrUndeletableKind = errors.New("azure: the hub's role cannot delete this kind of resource")

// KindOf maps a resource type to the short kind the delete path speaks. The
// boolean says whether the three roles the hub holds can delete it: Virtual
// Machine Contributor deletes VMs, NICs and disks; Website Contributor
// deletes plans; nobody the hub is deletes a public IP.
func KindOf(resourceType string) (string, bool) {
	switch strings.ToLower(resourceType) {
	case "microsoft.compute/virtualmachines":
		return "vm", true
	case "microsoft.network/networkinterfaces":
		return "nic", true
	case "microsoft.compute/disks":
		return "disk", true
	case "microsoft.web/serverfarms":
		return "plan", true
	case "microsoft.network/publicipaddresses":
		return "ip", false
	}
	return "", false
}

func apiVersionFor(kind string) (string, error) {
	switch strings.ToLower(kind) {
	case "vm":
		return computeAPIVersion, nil
	case "nic":
		return networkAPIVersion, nil
	case "disk":
		return diskAPIVersion, nil
	case "plan":
		return webAPIVersion, nil
	}
	return "", fmt.Errorf("azure: nothing known about a resource of kind %q", kind)
}

// DeleteAny deletes one resource the person named. No ownership check: an
// orphan is by definition something nobody is using, whoever created it,
// and the typed name is the consent. A kind the role cannot delete is
// refused before any call, with a sentence instead of Azure's bare 403.
func DeleteAny(ctx context.Context, c *Client, armID, kind string) error {
	if kind == "ip" || kind == "" {
		return fmt.Errorf("%w: %s", ErrUndeletableKind, armID)
	}
	version, err := apiVersionFor(kind)
	if err != nil {
		return err
	}
	resp, err := c.DeleteAsync(ctx, armID, url.Values{"api-version": {version}})
	if err != nil {
		if StatusOf(err) == http.StatusNotFound {
			return nil
		}
		return err
	}
	return Await(ctx, c, resp)
}
```

In `guardrails_hub.go`:

```go
// DeleteOrphan is the second destructive call of the API, and it has the same
// shape as the first: the name typed back, or nothing happens.
func (h *Hub) DeleteOrphan(resourceID, confirmName string) error {
	_, client, ok := h.azureReady()
	if !ok {
		return ErrAzureOff
	}
	r, err := h.st.ActionableAzureResource(azure.NormalizeID(resourceID))
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmName) != r.Name {
		return &azure.Refusal{Err: fmt.Errorf("%w: type %q to confirm", ErrWrongName, r.Name)}
	}
	kind, deletable := azure.KindOf(r.Type)
	if !deletable {
		return &azure.Refusal{Err: fmt.Errorf("%w: %s (%s)", azure.ErrUndeletableKind, r.Name, r.Type)}
	}
	armID := r.ARMID
	if armID == "" {
		armID = r.ID
	}
	ctx, cancel := context.WithTimeout(h.baseCtx(), 10*time.Minute)
	defer cancel()
	if err := azure.DeleteAny(ctx, client, armID, kind); err != nil {
		return err
	}
	h.kickAzure() // the next sweep drops the row and resolves the orphan event
	return nil
}
```

Note that `DeleteOrphan` is synchronous (a disk or NIC deletes in seconds; a plan too). If the 60-second HTTP budget of the handler is a concern, keep it synchronous anyway and document it: a modal that says "deleting…" and then "gone" is what the person expects.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/... -race`
Expected: PASS.

- [ ] **Step 5: Mutation check**

In `DeleteAny`, remove the `kind == "ip"` guard; `TestDeleteAnyDoesNotCheckOwnershipAndTreats404AsDone` fails on "ip must be refused". Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/azure/delete.go internal/hub/azure/delete_test.go internal/hub/guardrails_hub.go internal/hub/guardrails_hub_test.go internal/hub/azure_test.go
git commit -m "feat(azure): delete an orphan by name — disks, NICs, plans; public IPs refused up front"
```

---

### Task 10: API routes

**Files:**
- Create: `internal/hub/server/api_guardrails.go`, `internal/hub/server/api_guardrails_test.go`
- Modify: `internal/hub/server/server.go` (register routes)
- Modify: `internal/hub/server/api_azure.go` (`Azurer` gains `DeleteOrphan(resourceID, confirmName string) error` and `GuardrailSettings() guardrails.Settings`; the action view gains `origin`)

**Interfaces:**
- Produces the JSON of spec §4:
  ```go
  type guardrailsView struct {
      Budget      float64            `json:"budget"`
      Spent       float64            `json:"spent"`
      Currencies  []string           `json:"currencies"`
      Projection  *float64           `json:"projection"`   // null before day 4
      DaysBilled  int                `json:"days_billed"`
      Thresholds  []thresholdView    `json:"thresholds"`   // {pct, line, firing}
      Shares      []shareView        `json:"shares"`       // {resource_id, name, amount, share_pct, firing}
      Orphans     []orphanView       `json:"orphans"`      // {resource_id, name, type, reason, since, cost, currency, deletable}
      Events      []guardrailEventView `json:"events"`     // last 20: {subject, name, rule, detail, kind, value, at}
      Timezone    string             `json:"timezone"`
  }
  type guardrailSettingsView struct {
      Thresholds      []int  `json:"thresholds"`
      ResourceShare   int    `json:"resource_share_pct"`
      HubVMSilentDays int    `json:"hub_vm_silent_days"`
      Timezone        string `json:"timezone"`
  }
  type scheduleView struct {
      ResourceID   string             `json:"resource_id"`
      Name         string             `json:"name"`
      OffWindows   []guardrails.Window `json:"off_windows"`
      Enabled      bool               `json:"enabled"`
      LastBoundary *string            `json:"last_boundary"`
      OffNow       bool               `json:"off_now"`
  }
  ```
  Routes: `GET /api/v1/azure/guardrails`, `GET|PUT /api/v1/azure/guardrails/settings`, `GET /api/v1/azure/schedules`, `PUT /api/v1/azure/schedules` (body `{resource_id, off_windows, enabled}`), `POST /api/v1/azure/schedules/delete` (`{resource_id}`), `POST /api/v1/azure/orphans/delete` (`{resource_id, confirm_name}`).

- [ ] **Step 1: Write the failing tests**

Read `internal/hub/server/api_azure_test.go` first for the server fixture (`newTestServer`, the fake `Azurer`, how a session cookie is obtained) and mirror it.

```go
// internal/hub/server/api_guardrails_test.go
package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGuardrailsViewComputesFromTheStore(t *testing.T) {
	s, st, cookie := newTestServerWithAzure(t) // the api_azure_test fixture; returns the store too
	now := time.Now().UTC()
	_ = st.SetSetting("azure_budget_monthly", "100")
	_ = st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/d1", ARMID: "/S/d1", Name: "d1", Type: "Microsoft.Compute/disks", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/ip1", ARMID: "/S/ip1", Name: "ip1", Type: "Microsoft.Network/publicIPAddresses", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now)
	_ = st.SetAzureOrphans(map[string]string{"/s/d1": "disk_unattached", "/s/ip1": "ip_unassociated"}, now)
	_ = st.UpsertAzureCosts([]store.AzureCost{{ResourceID: "/s/d1", Period: now.Format("2006-01"), Amount: 40, Currency: "EUR", AsOf: now}})
	_, _ = st.InsertGuardrailEvent(store.GuardrailEvent{Subject: "budget", Rule: "budget_threshold", Detail: "80", Kind: "fired", Value: 85, At: now})

	var v map[string]any
	getJSON(t, s, cookie, "/api/v1/azure/guardrails", &v)
	if v["budget"].(float64) != 100 || v["spent"].(float64) != 40 {
		t.Fatalf("totals: %v", v)
	}
	orphans := v["orphans"].([]any)
	if len(orphans) != 2 {
		t.Fatalf("orphans: %v", orphans)
	}
	for _, o := range orphans {
		m := o.(map[string]any)
		if m["name"] == "ip1" && m["deletable"] != false {
			t.Fatal("a public IP is not deletable")
		}
		if m["name"] == "d1" && (m["deletable"] != true || m["cost"].(float64) != 40) {
			t.Fatalf("disk: %v", m)
		}
	}
	th := v["thresholds"].([]any)[0].(map[string]any)
	if th["pct"].(float64) != 80 || th["firing"] != true {
		t.Fatalf("threshold 80 must show firing from the journal: %v", th)
	}
	if v["projection"] != nil && now.Day() < 5 {
		t.Fatal("no projection before day 4")
	}
}

func TestGuardrailSettingsRoundTripAndRefusal(t *testing.T) {
	s, _, cookie := newTestServerWithAzure(t)
	if code := putJSON(t, s, cookie, "/api/v1/azure/guardrails/settings", `{"thresholds":[50,90],"resource_share_pct":25,"hub_vm_silent_days":2,"timezone":"Europe/Paris"}`); code != http.StatusNoContent {
		t.Fatalf("put = %d", code)
	}
	var v map[string]any
	getJSON(t, s, cookie, "/api/v1/azure/guardrails/settings", &v)
	if v["resource_share_pct"].(float64) != 25 || v["timezone"] != "Europe/Paris" {
		t.Fatalf("got %v", v)
	}
	code, body := putJSONBody(t, s, cookie, "/api/v1/azure/guardrails/settings", `{"thresholds":[50],"resource_share_pct":25,"hub_vm_silent_days":2,"timezone":"Mars/Olympus"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "Mars/Olympus") {
		t.Fatalf("refusal = %d %s", code, body)
	}
}

func TestSchedulesRoutes(t *testing.T) {
	s, st, cookie := newTestServerWithAzure(t)
	now := time.Now().UTC()
	_ = st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}, now)
	body := `{"resource_id":"/s/site1","off_windows":[{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}],"enabled":true}`
	if code := putJSON(t, s, cookie, "/api/v1/azure/schedules", body); code != http.StatusNoContent {
		t.Fatalf("put = %d", code)
	}
	// A type lot 4 cannot stop is refused with the type in the message.
	_ = st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/vnet", ARMID: "/S/vnet", Name: "vnet", Type: "Microsoft.Network/virtualNetworks", ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}}}, now)
	code, msg := putJSONBody(t, s, cookie, "/api/v1/azure/schedules", `{"resource_id":"/s/vnet","off_windows":[],"enabled":true}`)
	if code != http.StatusBadRequest || !strings.Contains(msg, "virtualNetworks") {
		t.Fatalf("vnet: %d %s", code, msg)
	}
	var list map[string][]map[string]any
	getJSON(t, s, cookie, "/api/v1/azure/schedules", &list)
	if len(list["schedules"]) != 1 || list["schedules"][0]["name"] != "site1" {
		t.Fatalf("list = %v", list)
	}
	if code := postJSON(t, s, cookie, "/api/v1/azure/schedules/delete", `{"resource_id":"/s/site1"}`); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
}

func TestOrphanDeleteRouteCarriesTheRefusal(t *testing.T) {
	s, _, cookie := newTestServerWithAzure(t) // its fake Azurer's DeleteOrphan returns a Refusal for name "nope"
	code, body := postJSONBody(t, s, cookie, "/api/v1/azure/orphans/delete", `{"resource_id":"/s/d1","confirm_name":"nope"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "type") {
		t.Fatalf("got %d %s", code, body)
	}
}
```

Write the small `getJSON`/`putJSON`/`putJSONBody`/`postJSON`/`postJSONBody` helpers beside the fixture if they do not exist; keep them in one `helpers_test.go`.

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/hub/server/ -run 'Guardrail|SchedulesRoutes|OrphanDelete' -v`
Expected: FAIL — 404 on every route.

- [ ] **Step 3: Implement**

`api_guardrails.go`: one handler per route. `handleGetGuardrails` computes `spent` and rows exactly as `handleGetAzure` does (extract the shared cost-join into a helper `costSummary(st, period) (spent float64, currencies []string, byID map[string]store.AzureCost, asOf time.Time, err error)` used by both); calls `guardrails.Projection(now, asOf, spent)`; builds `thresholds` from `Azure.GuardrailSettings().Thresholds` with `firing` read from `LastGuardrailEventPerKey`; `shares` from rows above the share line; `orphans` from `ListAzureResources` where `OrphanReason != ""` with `deletable` from `azure.KindOf` and `false` for `unverified`; `events` from `ListGuardrailEvents(20)` with names resolved through the resource list. `handlePutGuardrailSettings` decodes, calls `Validate`, `SaveSettings`, 204. `handlePutSchedule` decodes, looks the resource up with `ActionableAzureResource`, refuses with 400 `"a schedule needs a type the hub can stop; <type> is not one"` when `!azure.Supports(r.Type, azure.ActionStop)`, validates windows with `ParseWindows`, stores `EncodeWindows(ws)`. `handleDeleteOrphan` maps `store.ErrNotFound` → 404, `azure.IsRefusal` → 400 with the message, `ErrAzureOff` → 400, else 502 with Azure's words (`writeErr(w, 502, err.Error())`).

Register in `server.go` next to the other Azure routes. Add `Origin string `json:"origin"`` to the action view in `api_azure.go`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/server/ -race`
Expected: PASS.

- [ ] **Step 5: Mutation check**

In `handlePutSchedule`, remove the `Supports` check; `TestSchedulesRoutes` fails on the vnet case. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/server/
git commit -m "feat(api): guardrails state and settings, schedules, orphan deletion by name"
```

---

### Task 11: Front — helpers, page blocks, settings block

**Files:**
- Create: `web/src/lib/guardrails.ts`, `web/src/lib/guardrails.test.ts`
- Modify: `web/src/lib/api.ts` (types `GuardrailsView`, `GuardrailSettings`, `Schedule`, `Window`; `AzureAction.origin`)
- Modify: `web/src/routes/azure/+page.svelte` (three blocks + schedule column + origin in the actions table)
- Modify: `web/src/routes/settings/+page.svelte` (Guardrails block)

**Interfaces:**
- Produces (pure, tested):
  ```ts
  export function projectionText(view: GuardrailsView): string;   // "— (3 billed days)" | "€ 118 by month end"
  export function thresholdMarks(thresholds: {pct:number}[]): number[]; // percentages for the bar
  export function windowsSummary(ws: Window[]): string;            // "Mon–Fri 20:00–07:00, Sat–Sun all day"
  export function eveningsAndWeekends(): Window[];
  export function validWindows(ws: Window[]): string | null;       // null when valid, else the first problem
  export function orphanLabel(reason: string): string;             // "Unattached disk" …, "Not verified"
  ```

- [ ] **Step 1: Write the failing tests**

```ts
// web/src/lib/guardrails.test.ts
import { describe, expect, it } from 'vitest';
import { eveningsAndWeekends, orphanLabel, projectionText, thresholdMarks, validWindows, windowsSummary } from './guardrails';

describe('guardrails helpers', () => {
  it('summarises windows the way a person would say them', () => {
    expect(windowsSummary(eveningsAndWeekends())).toBe('Mon–Fri 20:00–07:00, Sat–Sun all day');
    expect(windowsSummary([{ days: [1, 3], from: '22:00', to: '06:00' }])).toBe('Mon, Wed 22:00–06:00');
    expect(windowsSummary([])).toBe('no window');
  });

  it('validates windows before they leave the form', () => {
    expect(validWindows(eveningsAndWeekends())).toBeNull();
    expect(validWindows([{ days: [], from: '20:00', to: '07:00' }])).toMatch(/day/);
    expect(validWindows([{ days: [1], from: '20:00', to: '7:00' }])).toMatch(/HH:MM/);
    expect(validWindows([{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: '24:00' }])).toMatch(/always off/);
  });

  it('renders the projection honestly', () => {
    expect(projectionText({ projection: null, days_billed: 3, currencies: ['EUR'] } as never)).toBe('— (3 billed days)');
    expect(projectionText({ projection: 118.4, days_billed: 10, currencies: ['EUR'] } as never)).toBe('€118 by month end');
    expect(projectionText({ projection: 118.4, days_billed: 10, currencies: ['EUR', 'USD'] } as never)).toMatch(/2 currencies/);
  });

  it('places threshold marks and names orphan reasons', () => {
    expect(thresholdMarks([{ pct: 80 }, { pct: 100 }, { pct: 150 }])).toEqual([80, 100]); // beyond the bar is not drawn
    expect(orphanLabel('disk_unattached')).toBe('Unattached disk');
    expect(orphanLabel('unverified')).toBe('Not verified');
    expect(orphanLabel('hub_vm_silent')).toBe('VM created by the hub, agent silent');
  });
});
```

- [ ] **Step 2: Run to see them fail**

Run: `cd web && npx vitest run src/lib/guardrails.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the helpers and types**

```ts
// web/src/lib/guardrails.ts
import { money } from './azure';
import type { GuardrailsView, Window } from './api';

const DAY = ['', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

export function eveningsAndWeekends(): Window[] {
  return [
    { days: [1, 2, 3, 4, 5], from: '20:00', to: '07:00' },
    { days: [6, 7], from: '00:00', to: '24:00' }
  ];
}

function dayRange(days: number[]): string {
  const d = [...days].sort((a, b) => a - b);
  const contiguous = d.every((x, i) => i === 0 || x === d[i - 1] + 1);
  if (contiguous && d.length > 2) return `${DAY[d[0]]}–${DAY[d[d.length - 1]]}`;
  if (contiguous && d.length === 2) return `${DAY[d[0]]}–${DAY[d[1]]}`;
  return d.map((x) => DAY[x]).join(', ');
}

export function windowsSummary(ws: Window[]): string {
  if (ws.length === 0) return 'no window';
  return ws
    .map((w) => `${dayRange(w.days)} ${w.from === '00:00' && w.to === '24:00' ? 'all day' : `${w.from}–${w.to}`}`)
    .join(', ');
}

const HHMM = /^([01]\d|2[0-4]):[0-5]\d$/;

export function validWindows(ws: Window[]): string | null {
  for (const w of ws) {
    if (!w.days || w.days.length === 0) return 'every window needs at least one day';
    if (!HHMM.test(w.from) || !HHMM.test(w.to)) return 'times are HH:MM';
    if (w.from === '24:00') return 'a window cannot start at 24:00';
  }
  const full = ws.some((w) => w.from === '00:00' && w.to === '24:00' && w.days.length === 7);
  if (full) return 'this resource would be always off — fine if that is what you want';
  return null;
}

export function projectionText(v: GuardrailsView): string {
  if (v.projection === null || v.projection === undefined) return `— (${v.days_billed} billed days)`;
  const cur = v.currencies.length === 1 ? v.currencies[0] : undefined;
  const text = `${money(Math.round(v.projection), cur)} by month end`;
  return v.currencies.length > 1 ? `${text} (${v.currencies.length} currencies summed)` : text;
}

export function thresholdMarks(th: { pct: number }[]): number[] {
  return th.map((t) => t.pct).filter((p) => p <= 100);
}

const LABELS: Record<string, string> = {
  disk_unattached: 'Unattached disk',
  ip_unassociated: 'Public IP not associated',
  nic_without_vm: 'Network interface without a VM',
  plan_without_site: 'App Service plan without a site',
  hub_vm_silent: 'VM created by the hub, agent silent',
  unverified: 'Not verified'
};
export function orphanLabel(reason: string): string {
  return LABELS[reason] ?? reason;
}
```

Check `money(amount, currency)`'s exact output for `(118, 'EUR')` in `azure.ts` and align the expected string in the test with it (the test says `€118`; if `money` renders `118,00 €`, change the test to the real format — the helper is not the place to invent a second money format).

In `api.ts`, add:

```ts
export interface Window { days: number[]; from: string; to: string }
export interface GuardrailsView {
  budget: number; spent: number; currencies: string[];
  projection: number | null; days_billed: number;
  thresholds: { pct: number; line: number; firing: boolean }[];
  shares: { resource_id: string; name: string; amount: number; share_pct: number; firing: boolean }[];
  orphans: { resource_id: string; name: string; type: string; reason: string; since: string; cost: number | null; currency?: string; deletable: boolean }[];
  events: { subject: string; name: string; rule: string; detail: string; kind: string; value: number; at: string }[];
  timezone: string;
}
export interface GuardrailSettings { thresholds: number[]; resource_share_pct: number; hub_vm_silent_days: number; timezone: string }
export interface Schedule { resource_id: string; name: string; off_windows: Window[]; enabled: boolean; last_boundary: string | null; off_now: boolean }
```
and `origin: string` on `AzureAction` in `azure.ts`.

- [ ] **Step 4: Run the helper tests**

Run: `cd web && npx vitest run src/lib/guardrails.test.ts`
Expected: PASS.

- [ ] **Step 5: The page**

In `web/src/routes/azure/+page.svelte`:
- Load `GuardrailsView` from `/api/v1/azure/guardrails` and `Schedule[]` from `/api/v1/azure/schedules` in `load()`; reload both on the `azure` live event (the existing subscription).
- **Budget block** (replace the current bar): the bar with `thresholdMarks` drawn as thin vertical lines (`absolute` divs at `left: {pct}%`), the projection line from `projectionText`, class `bg-red-600` on the bar when any threshold or the projection is `firing`. Under it, a list of `shares` with `firing` rows in red.
- **Orphans block**: table reason (`orphanLabel`) / name (+ short type) / since (`fmtAgo`) / cost (`money`) / *Delete* button when `deletable`. The button opens the existing confirm-by-name modal (`confirming`/`typedName` state already on the page for provisions): generalise that state to `{ kind: 'provision' | 'orphan', id, name }`, and post to `/api/v1/azure/orphans/delete` with `{resource_id, confirm_name}` for orphans. Error from the server shown in the modal, row kept.
- **Schedules**: a new column in the resource table: for rows whose type supports `stop` (`actionsFor(r.type).includes('stop')`), a clock icon button; enabled schedules show `windowsSummary` in `title`. Clicking opens an inline editor (one at a time, like `confirming`): seven checkboxes + two `<input type="time">`, a second row for a second window, an *Evenings + weekends* button that sets `eveningsAndWeekends()`, `validWindows` message shown live, *Save* → `PUT /api/v1/azure/schedules`, *Remove* → `POST /api/v1/azure/schedules/delete`. For VMs, the stop button label in the actions column reads *Deallocate*.
- **Recent actions**: an *origin* column (`user` / `schedule`).
- **Guardrail events**: under the orphans, the last 20 events as a small list (`name · rule detail · kind · ago`), the same style as the alert log.

In `web/src/routes/settings/+page.svelte`, Azure section: a *Guardrails* sub-block with thresholds (comma-separated text input), resource share (number), silent-VM days (number), timezone (text with `Europe/Paris` placeholder); loaded from and saved to `/api/v1/azure/guardrails/settings`, errors shown inline (the zone name comes back in the message).

- [ ] **Step 6: Check and test the front**

Run: `cd web && npm run check && npm test -- --run`
Expected: 0 errors, all tests green.

- [ ] **Step 7: Manual pass against a real hub**

`make web build && SM_LISTEN=:8091 SM_DATA_DIR=/tmp/sm-lot6 ./bin/smhub` — log in (Vincent creates the account; do not type a password), open the Azure page with Azure off: the three blocks render their empty states without console errors. Screenshot for the PR.

- [ ] **Step 8: Commit**

```bash
git add web/
git commit -m "feat(web): budget marks and projection, orphans with delete by name, schedule editor"
```

---

### Task 12: Docs, CHANGELOG, and the real proof scripts

**Files:**
- Modify: `README.md` (features table, roadmap row 6 → **done**, "What this does not do yet": drop the guardrails line, add "no automatic deletion", "one time zone")
- Modify: `docs/ARCHITECTURE_EN.md` and `docs/ARCHITECTURE.md` (same edits, same commit: the guardrails package in the component table, the two new tables, the boundary-crossing rule, the deliveries table pointing at two journals)
- Modify: `CHANGELOG.md` (Unreleased → the lot 6 entries)
- Create: `deploy/azure/proof-lot6.md` — the two real proofs Vincent runs by hand, as scripts to paste: (1) a schedule on `webapp-vlauriat-probe-09121238` with a ten-minute window starting five minutes from now, then `az monitor activity-log list` filtered on `sites/stop` and `sites/start` with the caller; (2) `az disk create -g rg-dev-vincent-sandbox -n sm-orphan-proof --size-gb 4 --sku Standard_LRS`, wait one inventory sweep, see it under Orphans with cost `—`, delete it by name from the page, `az disk show` → not found.

- [ ] **Step 1: Write the docs**

Keep the README's tone: one paragraph per guardrail under a "Cost guardrails" heading, each ending with what it refuses to do. In ARCHITECTURE_EN, add a "Guardrails" section after "Azure actions" with the data flow (sync → evaluator → journal → dispatcher) and the three invariants (evaluate only after success; act on boundaries only; delete by name only); mirror it in French in the same commit.

- [ ] **Step 2: Full verification**

Run: `go vet ./... && go test ./... -race && cd web && npm run check && npm test -- --run`
Expected: all green.

- [ ] **Step 2b: Write the deploy step down, because nothing else in this plan does**

The hub running on `vm-serversmonitor` is on image `a3f1a1c-amd64`, which is at
**schema version 5**. Migration 6 has only ever been applied to a copy. Nothing in
tasks 1–12 deploys anything, so the first time migration 6 touches that VM will be
whenever somebody swaps the image — five schema changes plus a rebuild of the
`deliveries` table, in one go, on a machine Vincent may by then have created an
admin account on and pointed at his real subscription.

That must be a decision, not a side effect of a container restart. Add to
`deploy/azure/proof-lot6.md` a short "Deploying lot 6" section, before the two
proofs, saying in order: stop the container, copy `serversmonitor.db` off the VM
first (it is the only copy of the admin account and the alert history), load the
new image, start it, and check the log line reports `schema=6`. Name the rollback:
the old image is still in `docker images`, but a database migrated to 6 will not
open on the old binary, so rolling back means restoring the copy too.

- [ ] **Step 3: Commit and open the PR**

```bash
git add README.md docs/ CHANGELOG.md deploy/azure/proof-lot6.md
git commit -m "docs: lot 6 — cost guardrails"
git push -u origin feat/lot6-cost-guardrails
gh pr create --title "feat: lot 6 — cost guardrails (budget alerts, orphans, stop schedules)" --body-file docs/superpowers/plans/2026-09-23-lot6-pr-body.md
```

Write the PR body as the plan's summary: what each guardrail does, the invariants, the two real proofs still to be run by Vincent, and the three commits the classifier will not let Claude run.

---

## Self-review

**Spec coverage.** §0 decisions → Tasks 4, 6, 8, 9 (delivery through lot 2: Task 6; schedules through lot 4: Task 8; orphans in inventory: Tasks 5–6; delete by name: Task 9; one zone: Task 3; boundary crossing: Task 8; one budget: Task 4). §1 data model → Tasks 1–3 (the `deallocate` verb already exists; see Global Constraints). §2 evaluator → Tasks 4–6, including 403/404 and "no evaluation after a failed sync". §3 schedules → Tasks 7–8, including catch-up, no retry, DST, in-flight lock. §4 API and page → Tasks 10–11. §5 errors → the zone fallback is Task 3's `Location()`, the rest sits in the task named for each case. §6 tests → every task has unit + mutation; the real proofs are Task 12's scripts. §7 out of scope → nothing in the plan builds any of it.

**Placeholders.** Task 2's `SetAzureOrphans` snippet is explicitly a shape with the real algorithm spelled out in prose beneath it — the one place the plan describes rather than shows, because the trap it avoids is the point. Task 7's `clock` now carries the real implementation with both daylight-saving cases explained. Task 8's catch-up test is a table with exact instants (Friday 20:00 UTC is the merged weekend boundary). No "TBD", no "adjust until it passes".

**Failure surfaces the spec promised and the first draft missed**, added after review: the orphan sweep has its own `azure_sync` scope so one failed typed read cannot make the inventory look stale or clear yesterday's reasons (Task 6); `schedule_failed` fires for a boundary that produces no action row at all, which is the likeliest scheduling failure and the one `finishAction` can never see (Task 8); migration 6 is exercised against a copy of the production database before it ever runs on the VM (Task 1, step 7).

**Type consistency.** `store.GuardrailKey{Subject, Rule, Detail}` and `store.GuardrailEvent` are used identically in Tasks 1, 4, 6, 8, 10. `guardrails.Settings` fields match between Tasks 3, 6, 8, 10. `azure.KindOf` / `DeleteAny` / `ErrUndeletableKind` match between Tasks 9 and 10. `StartActionFrom(id, action, origin)` is defined in Task 8 and used only there. `Delivery.EventID *int64` / `GuardrailEventID *int64` are set in Task 1 and read in Task 6.
