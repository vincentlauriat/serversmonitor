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

func scanGuardrail(row scanner) (GuardrailEvent, error) {
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
