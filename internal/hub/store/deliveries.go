package store

import (
	"database/sql"
	"errors"
	"time"
)

// Delivery is one attempt to tell someone about one transition, through one
// channel — an alert transition or a guardrail transition, never both, which
// is what the CHECK constraint on the table enforces. The row exists before
// the first attempt, which is what makes the queue survive a restart.
type Delivery struct {
	ID               int64
	EventID          *int64
	GuardrailEventID *int64
	Channel          string
	State            string
	Attempts         int
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const deliveryCols = `id, event_id, guardrail_event_id, channel, state, attempts, last_error, created_at, updated_at`

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	var eventID, guardrailEventID sql.NullInt64
	var created, updated string
	err := row.Scan(&d.ID, &eventID, &guardrailEventID, &d.Channel, &d.State, &d.Attempts, &d.LastError, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if eventID.Valid {
		d.EventID = &eventID.Int64
	}
	if guardrailEventID.Valid {
		d.GuardrailEventID = &guardrailEventID.Int64
	}
	if d.CreatedAt, err = parseTime(created); err != nil {
		return d, err
	}
	d.UpdatedAt, err = parseTime(updated)
	return d, err
}

// CreateDelivery records the intent to deliver, before anything is attempted.
// A crash between here and the first send leaves a pending row, not silence.
func (s *Store) CreateDelivery(eventID int64, channel string, now time.Time) (Delivery, error) {
	at := fmtTime(now)
	res, err := s.db.Exec(`INSERT INTO deliveries(event_id, channel, state, created_at, updated_at) VALUES (?,?,'pending',?,?)`,
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
	return s.execOne(`UPDATE deliveries SET state='sent', attempts=?, last_error='', updated_at=? WHERE id=?`,
		attempts, fmtTime(now), id)
}

func (s *Store) MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error {
	return s.execOne(`UPDATE deliveries SET state='failed', attempts=?, last_error=?, updated_at=? WHERE id=?`,
		attempts, reason, fmtTime(now), id)
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

// PendingDeliveries returns, oldest first, what a previous run left unfinished.
func (s *Store) PendingDeliveries() ([]Delivery, error) {
	return s.queryDeliveries(`SELECT ` + deliveryCols + ` FROM deliveries WHERE state='pending' ORDER BY id`)
}

// ListDeliveries returns the most recent deliveries, newest first.
func (s *Store) ListDeliveries(limit int) ([]Delivery, error) {
	return s.queryDeliveries(`SELECT `+deliveryCols+` FROM deliveries ORDER BY id DESC LIMIT ?`, limit)
}

// DeliveryEvent returns what a delivery is about: its event, and that event's
// host. A delivery about a guardrail transition, not an alert, answers
// ErrNotFound here — the caller must ask DeliveryGuardrailEvent instead.
func (s *Store) DeliveryEvent(id int64) (AlertEvent, Host, error) {
	var eventID sql.NullInt64
	err := s.db.QueryRow(`SELECT event_id FROM deliveries WHERE id = ?`, id).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !eventID.Valid) {
		return AlertEvent{}, Host{}, ErrNotFound
	}
	if err != nil {
		return AlertEvent{}, Host{}, err
	}
	e, err := scanEvent(s.db.QueryRow(`SELECT id, rule_id, host_id, metric, kind, value, at FROM alert_events WHERE id = ?`, eventID.Int64))
	if errors.Is(err, sql.ErrNoRows) {
		return AlertEvent{}, Host{}, ErrNotFound
	}
	if err != nil {
		return AlertEvent{}, Host{}, err
	}
	h, err := s.Host(e.HostID)
	return e, h, err
}

// ChannelHealth returns the most recent delivery of each channel. The settings
// page shows it because "configured" and "working" are different claims.
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
