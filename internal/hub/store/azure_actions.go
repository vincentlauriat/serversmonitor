package store

import (
	"database/sql"
	"errors"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// AzureAction is one start, stop or restart, recorded before it was attempted.
type AzureAction struct {
	ID           int64
	ResourceID   string
	ResourceName string
	Action       string
	Status       string // pending | running | succeeded | failed | interrupted
	RequestedAt  time.Time
	FinishedAt   *time.Time
	Error        string
	StateBefore  *string
	StateAfter   *string
}

var (
	ErrActionInFlight = errors.New("store: an action is already running on this resource")
	ErrNoSuchResource = errors.New("store: no such live Azure resource")
)

// ActionableAzureResource returns a resource only if the last sync still saw
// it. ListAzureResources deliberately returns soft-deleted rows so the page can
// show them; acting on one is another matter — Azure no longer has it.
func (s *Store) ActionableAzureResource(id string) (AzureResource, error) {
	rs, err := s.ListAzureResources()
	if err != nil {
		return AzureResource{}, err
	}
	for _, r := range rs {
		if r.ID == id && r.DeletedAt == nil {
			return r, nil
		}
	}
	return AzureResource{}, ErrNoSuchResource
}

// StartAzureAction writes the pending row. The row exists before the call, so a
// crash mid-flight leaves a trace rather than silence.
func (s *Store) StartAzureAction(a AzureAction) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO azure_actions
		(resource_id, resource_name, action, status, requested_at, state_before)
		VALUES (?,?,?,'pending',?,?)`,
		a.ResourceID, a.ResourceName, a.Action, fmtTime(a.RequestedAt), nullString(a.StateBefore))
	if err != nil {
		// Read the driver's code, never the English in its message: the partial
		// unique index is what refuses a second action on the same resource.
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return 0, ErrActionInFlight
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) MarkAzureActionRunning(id int64) error {
	_, err := s.db.Exec(`UPDATE azure_actions SET status='running' WHERE id = ? AND status='pending'`, id)
	return err
}

// FinishAzureAction closes the row. stateAfter stays NULL when the read-back
// failed or Azure said nothing: an unread state is never a stated one.
func (s *Store) FinishAzureAction(id int64, status, errMsg string, stateAfter *string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE azure_actions
		SET status = ?, error = ?, state_after = ?, finished_at = ?
		WHERE id = ?`, status, errMsg, nullString(stateAfter), fmtTime(now), id)
	return err
}

// InterruptAzureActions closes what was in flight when the hub died, and never
// replays it. Re-firing a stop at startup could stop a resource restarted by
// hand in the meantime; "nobody knows" is the honest record, and the next
// inventory sync says what actually happened.
func (s *Store) InterruptAzureActions(now time.Time) (int, error) {
	res, err := s.db.Exec(`UPDATE azure_actions
		SET status = 'interrupted', finished_at = ?
		WHERE status IN ('pending','running')`, fmtTime(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) ListAzureActions(limit int) ([]AzureAction, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, resource_id, resource_name, action, status,
		requested_at, finished_at, error, state_before, state_after
		FROM azure_actions ORDER BY requested_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureAction
	for rows.Next() {
		var a AzureAction
		var requested string
		var finished, before, after sql.NullString
		if err := rows.Scan(&a.ID, &a.ResourceID, &a.ResourceName, &a.Action, &a.Status,
			&requested, &finished, &a.Error, &before, &after); err != nil {
			return nil, err
		}
		if a.RequestedAt, err = parseTime(requested); err != nil {
			return nil, err
		}
		if finished.Valid {
			t, err := parseTime(finished.String)
			if err != nil {
				return nil, err
			}
			a.FinishedAt = &t
		}
		a.StateBefore = optString(before)
		a.StateAfter = optString(after)
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func optString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
