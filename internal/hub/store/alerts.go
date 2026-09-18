package store

import (
	"database/sql"
	"time"
)

// Rule fires when its metric goes above Threshold for Duration.
// HostID nil means the rule applies to every host.
type Rule struct {
	ID        int64
	HostID    *int64
	Metric    string
	Threshold float64
	Duration  time.Duration
	CreatedAt time.Time
}

// AlertEvent is one transition. Only fired and resolved are ever stored.
type AlertEvent struct {
	ID     int64
	RuleID int64
	HostID int64
	Metric string
	Kind   string
	Value  float64
	At     time.Time
}

func (s *Store) CreateRule(r Rule, now time.Time) (Rule, error) {
	res, err := s.db.Exec(`INSERT INTO alert_rules(host_id, metric, threshold, duration_sec, created_at) VALUES (?,?,?,?,?)`,
		r.HostID, r.Metric, r.Threshold, int64(r.Duration/time.Second), fmtTime(now))
	if err != nil {
		return r, err
	}
	r.ID, _ = res.LastInsertId()
	r.CreatedAt = now
	return r, nil
}

func (s *Store) UpdateRule(r Rule) error {
	return s.execOne(`UPDATE alert_rules SET host_id=?, metric=?, threshold=?, duration_sec=? WHERE id=?`,
		r.HostID, r.Metric, r.Threshold, int64(r.Duration/time.Second), r.ID)
}

func (s *Store) DeleteRule(id int64) error {
	return s.execOne(`DELETE FROM alert_rules WHERE id = ?`, id)
}

func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(`SELECT id, host_id, metric, threshold, duration_sec, created_at FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var hostID sql.NullInt64
		var sec int64
		var created string
		if err := rows.Scan(&r.ID, &hostID, &r.Metric, &r.Threshold, &sec, &created); err != nil {
			return nil, err
		}
		if hostID.Valid {
			v := hostID.Int64
			r.HostID = &v
		}
		r.Duration = time.Duration(sec) * time.Second
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedDefaultRules installs the spec defaults when no rule exists yet.
func (s *Store) SeedDefaultRules(now time.Time) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM alert_rules`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	defaults := []Rule{
		{Metric: "cpu", Threshold: 90, Duration: 10 * time.Minute},
		{Metric: "memory", Threshold: 90, Duration: 10 * time.Minute},
		{Metric: "disk", Threshold: 90, Duration: 30 * time.Minute},
		{Metric: "temperature", Threshold: 80, Duration: 5 * time.Minute},
	}
	for _, r := range defaults {
		if _, err := s.CreateRule(r, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) InsertAlertEvent(e AlertEvent) (AlertEvent, error) {
	res, err := s.db.Exec(`INSERT INTO alert_events(rule_id, host_id, metric, kind, value, at) VALUES (?,?,?,?,?,?)`,
		e.RuleID, e.HostID, e.Metric, e.Kind, e.Value, fmtTime(e.At))
	if err != nil {
		return e, err
	}
	e.ID, _ = res.LastInsertId()
	return e, nil
}

func scanEvent(row scanner) (AlertEvent, error) {
	var e AlertEvent
	var at string
	if err := row.Scan(&e.ID, &e.RuleID, &e.HostID, &e.Metric, &e.Kind, &e.Value, &at); err != nil {
		return e, err
	}
	var err error
	e.At, err = parseTime(at)
	return e, err
}

func (s *Store) ListAlertEvents(hostID *int64, limit, offset int) ([]AlertEvent, error) {
	q := `SELECT id, rule_id, host_id, metric, kind, value, at FROM alert_events`
	args := []any{}
	if hostID != nil {
		q += ` WHERE host_id = ?`
		args = append(args, *hostID)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastEventPerKey returns the most recent event for every (rule, host) pair.
// The grouped query keeps this O(pairs) instead of reading the whole append-only
// log: it runs on every dashboard refresh, and the log only grows.
func (s *Store) LastEventPerKey() (map[[2]int64]AlertEvent, error) {
	rows, err := s.db.Query(`SELECT e.id, e.rule_id, e.host_id, e.metric, e.kind, e.value, e.at
		FROM alert_events e
		JOIN (SELECT rule_id, host_id, max(id) AS id FROM alert_events GROUP BY rule_id, host_id) last
		  ON last.id = e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]AlertEvent{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out[[2]int64{e.RuleID, e.HostID}] = e
	}
	return out, rows.Err()
}
