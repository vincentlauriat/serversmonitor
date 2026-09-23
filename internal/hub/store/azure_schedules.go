package store

import (
	"database/sql"
	"time"
)

// AzureSchedule is one resource's off-hours window, applied by the agent
// loop rather than by a person. LastBoundary is nil until the schedule has
// ever crossed a window boundary the loop acted on.
type AzureSchedule struct {
	ResourceID    string
	OffWindows    string // JSON, opaque to the store
	Enabled       bool
	LastBoundary  *time.Time
	LastAppliedAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UpsertAzureSchedule writes the windows and the enabled flag. It never
// touches last_boundary or last_applied_at: editing a schedule must not
// replay the action the agent loop already took at the last boundary.
func (s *Store) UpsertAzureSchedule(sc AzureSchedule, now time.Time) error {
	ts := fmtTime(now)
	_, err := s.db.Exec(`INSERT INTO azure_schedules (resource_id, off_windows, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(resource_id) DO UPDATE SET
		  off_windows = excluded.off_windows, enabled = excluded.enabled, updated_at = excluded.updated_at`,
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
	rows, err := s.db.Query(`SELECT resource_id, off_windows, enabled, last_boundary, last_applied_at, created_at, updated_at
		FROM azure_schedules ORDER BY resource_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureSchedule
	for rows.Next() {
		var sc AzureSchedule
		var enabled int
		var lb, la sql.NullString
		var created, updated string
		if err := rows.Scan(&sc.ResourceID, &sc.OffWindows, &enabled, &lb, &la, &created, &updated); err != nil {
			return nil, err
		}
		sc.Enabled = enabled == 1
		if sc.LastBoundary, err = parseNullTime(lb); err != nil {
			return nil, err
		}
		if sc.LastAppliedAt, err = parseNullTime(la); err != nil {
			return nil, err
		}
		if sc.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if sc.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// parseNullTime is parseTime for a column that may be NULL or empty, shared
// by every nullable RFC 3339 timestamp column in this package.
func parseNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkScheduleBoundary is written before the action is issued, so that a
// crash between the two never replays the action (lot 4's asymmetry).
func (s *Store) MarkScheduleBoundary(resourceID string, boundary, now time.Time) error {
	_, err := s.db.Exec(`UPDATE azure_schedules SET last_boundary = ?, last_applied_at = ? WHERE resource_id = ?`,
		fmtTime(boundary), fmtTime(now), resourceID)
	return err
}
