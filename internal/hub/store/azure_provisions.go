package store

import (
	"database/sql"
	"errors"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// AzureProvision is one attempt at creating a VM, recorded before the first
// call and carrying every resource it managed to create.
type AzureProvision struct {
	ID          int64
	Name        string
	HostID      *int64
	Status      string // pending | running | succeeded | failed | interrupted
	RequestedAt time.Time
	FinishedAt  *time.Time
	Error       string
	Resources   []AzureProvisionResource
}

// AzureProvisionResource is one resource that exists in Azure because of a
// provision. DeletedAt is set when a person deletes it — never by the hub on
// its own, not even to clean up after itself.
type AzureProvisionResource struct {
	ID        int64
	ARMID     string
	Kind      string // nic | vm | disk
	CreatedAt time.Time
	DeletedAt *time.Time
}

var (
	ErrProvisionInFlight = errors.New("store: a provision with this name is already running")
	ErrNoSuchProvision   = errors.New("store: no such provision")
)

// StartAzureProvision writes the pending row. It exists before the first call,
// so a crash mid-run leaves a trace rather than a resource nobody knows about.
func (s *Store) StartAzureProvision(name string, hostID *int64, now time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO azure_provisions (name, host_id, status, requested_at)
		VALUES (?,?,'pending',?)`, name, nullInt(hostID), fmtTime(now))
	if err != nil {
		// Read the driver's code, never the English in its message: the partial
		// unique index is what refuses a second run of the same name.
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return 0, ErrProvisionInFlight
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) MarkAzureProvisionRunning(id int64) error {
	_, err := s.db.Exec(`UPDATE azure_provisions SET status='running' WHERE id = ? AND status='pending'`, id)
	return err
}

// RecordAzureProvisionResource writes down a resource that now exists. Called
// as each one lands, before the next call is attempted: a crash between two
// PUTs must still leave the first one named.
func (s *Store) RecordAzureProvisionResource(provisionID int64, armID, kind string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO azure_provision_resources (provision_id, arm_id, kind, created_at)
		VALUES (?,?,?,?)`, provisionID, armID, kind, fmtTime(now))
	return err
}

func (s *Store) FinishAzureProvision(id int64, status, errMsg string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE azure_provisions SET status = ?, error = ?, finished_at = ?
		WHERE id = ?`, status, errMsg, fmtTime(now), id)
	return err
}

// InterruptAzureProvisions closes what was in flight when the hub died, and
// never resumes it. The resources it had already created stay recorded — that
// is the point: an interrupted provision may have created things, and they
// cost money until somebody looks.
func (s *Store) InterruptAzureProvisions(now time.Time) (int, error) {
	res, err := s.db.Exec(`UPDATE azure_provisions SET status = 'interrupted', finished_at = ?
		WHERE status IN ('pending','running')`, fmtTime(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// MarkAzureProvisionResourceDeleted records that a resource is gone. Deleting
// something already marked deleted is not an error: the second press of a
// button is not a failure.
func (s *Store) MarkAzureProvisionResourceDeleted(armID string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE azure_provision_resources SET deleted_at = ?
		WHERE arm_id = ? AND deleted_at IS NULL`, fmtTime(now), armID)
	return err
}

// AzureProvisionResourceByARMID finds a resource this hub created. The delete
// path goes through it, so a resource nobody recorded cannot be deleted by id.
func (s *Store) AzureProvisionResourceByARMID(armID string) (AzureProvisionResource, int64, error) {
	var r AzureProvisionResource
	var provisionID int64
	var created string
	var deleted sql.NullString
	err := s.db.QueryRow(`SELECT id, provision_id, arm_id, kind, created_at, deleted_at
		FROM azure_provision_resources WHERE arm_id = ?`, armID).
		Scan(&r.ID, &provisionID, &r.ARMID, &r.Kind, &created, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return AzureProvisionResource{}, 0, ErrNoSuchProvision
	}
	if err != nil {
		return AzureProvisionResource{}, 0, err
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return AzureProvisionResource{}, 0, err
	}
	if deleted.Valid {
		t, err := parseTime(deleted.String)
		if err != nil {
			return AzureProvisionResource{}, 0, err
		}
		r.DeletedAt = &t
	}
	return r, provisionID, nil
}

// ListAzureProvisions returns the runs newest first, each with its resources.
func (s *Store) ListAzureProvisions(limit int) ([]AzureProvision, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, name, host_id, status, requested_at, finished_at, error
		FROM azure_provisions ORDER BY requested_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AzureProvision{}
	byID := map[int64]int{}
	for rows.Next() {
		var p AzureProvision
		var hostID sql.NullInt64
		var requested string
		var finished sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &hostID, &p.Status, &requested, &finished, &p.Error); err != nil {
			return nil, err
		}
		if hostID.Valid {
			v := hostID.Int64
			p.HostID = &v
		}
		if p.RequestedAt, err = parseTime(requested); err != nil {
			return nil, err
		}
		if finished.Valid {
			t, err := parseTime(finished.String)
			if err != nil {
				return nil, err
			}
			p.FinishedAt = &t
		}
		p.Resources = []AzureProvisionResource{}
		byID[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	res, err := s.db.Query(`SELECT provision_id, id, arm_id, kind, created_at, deleted_at
		FROM azure_provision_resources ORDER BY provision_id, id`)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	for res.Next() {
		var provisionID int64
		var r AzureProvisionResource
		var created string
		var deleted sql.NullString
		if err := res.Scan(&provisionID, &r.ID, &r.ARMID, &r.Kind, &created, &deleted); err != nil {
			return nil, err
		}
		i, ok := byID[provisionID]
		if !ok {
			continue // an older run, outside the page being returned
		}
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if deleted.Valid {
			t, err := parseTime(deleted.String)
			if err != nil {
				return nil, err
			}
			r.DeletedAt = &t
		}
		out[i].Resources = append(out[i].Resources, r)
	}
	return out, res.Err()
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
