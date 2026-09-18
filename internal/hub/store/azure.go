package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// AzureResource is one resource as the last successful sync saw it.
// State is a pointer: NULL means nobody read it, which is never "stopped".
type AzureResource struct {
	ID                string
	Name              string
	Type              string
	ResourceGroup     string
	Location          string
	Kind              string
	SKU               string
	State             *string
	ProvisioningState string
	Host              string
	Tags              map[string]string
	FirstSeen         time.Time
	LastSeen          time.Time
	DeletedAt         *time.Time
}

// AzureCost is one resource's spend over one month. It carries no foreign key:
// a resource deleted before this hub first ran still cost money.
type AzureCost struct {
	ResourceID string
	Period     string
	Amount     float64
	Currency   string
	AsOf       time.Time
}

type AzureSync struct {
	Scope     string
	OK        bool
	Message   string
	StartedAt time.Time
	EndedAt   time.Time
}

// ReplaceAzureInventory upserts what the sync saw and soft-deletes, within the
// synced groups only, what it did not. One transaction: a half-applied sweep
// would leave resources marked deleted that are not.
//
// Call this only when the whole sync succeeded. A partial inventory would read
// as a batch of deletions, and "I cannot see Azure" would become "the sandbox
// is empty".
func (s *Store) ReplaceAzureInventory(groups []string, rs []AzureResource, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	at := fmtTime(now)
	for _, r := range rs {
		tags, err := json.Marshal(r.Tags)
		if err != nil {
			return err
		}
		var state any
		if r.State != nil {
			state = *r.State
		}
		// first_seen is kept from the existing row; last_seen always moves and
		// deleted_at is cleared, so a recreated resource comes back to life.
		if _, err := tx.Exec(`INSERT INTO azure_resources
			(id, name, type, resource_group, location, kind, sku, state, provisioning_state, host, tags, first_seen, last_seen, deleted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
			ON CONFLICT(id) DO UPDATE SET
			  name=excluded.name, type=excluded.type, resource_group=excluded.resource_group,
			  location=excluded.location, kind=excluded.kind, sku=excluded.sku,
			  state=excluded.state, provisioning_state=excluded.provisioning_state,
			  host=excluded.host, tags=excluded.tags, last_seen=excluded.last_seen, deleted_at=NULL`,
			r.ID, r.Name, r.Type, r.ResourceGroup, r.Location, r.Kind, r.SKU, state,
			r.ProvisioningState, r.Host, string(tags), at, at); err != nil {
			return err
		}
	}

	// The sweep, scoped to the groups this sync actually covered: syncing one
	// group must not mark another group's resources deleted.
	if len(groups) > 0 {
		q := `UPDATE azure_resources SET deleted_at = ?
		      WHERE deleted_at IS NULL AND last_seen < ? AND lower(resource_group) IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(groups)), ",") + `)`
		args := []any{at, at}
		for _, g := range groups {
			args = append(args, strings.ToLower(g))
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const azureResourceCols = `id, name, type, resource_group, location, kind, sku, state,
	provisioning_state, host, tags, first_seen, last_seen, deleted_at`

func (s *Store) ListAzureResources() ([]AzureResource, error) {
	rows, err := s.db.Query(`SELECT ` + azureResourceCols + ` FROM azure_resources ORDER BY resource_group, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureResource
	for rows.Next() {
		var r AzureResource
		var state, deleted sql.NullString
		var tags, first, last string
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.ResourceGroup, &r.Location, &r.Kind, &r.SKU,
			&state, &r.ProvisioningState, &r.Host, &tags, &first, &last, &deleted); err != nil {
			return nil, err
		}
		if state.Valid {
			v := state.String
			r.State = &v
		}
		if deleted.Valid {
			t, err := parseTime(deleted.String)
			if err != nil {
				return nil, err
			}
			r.DeletedAt = &t
		}
		if r.FirstSeen, err = parseTime(first); err != nil {
			return nil, err
		}
		if r.LastSeen, err = parseTime(last); err != nil {
			return nil, err
		}
		r.Tags = map[string]string{}
		json.Unmarshal([]byte(tags), &r.Tags)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertAzureCosts(cs []AzureCost) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range cs {
		if _, err := tx.Exec(`INSERT INTO azure_costs(resource_id, period, amount, currency, as_of)
			VALUES (?,?,?,?,?)
			ON CONFLICT(resource_id, period) DO UPDATE SET
			  amount=excluded.amount, currency=excluded.currency, as_of=excluded.as_of`,
			c.ResourceID, c.Period, c.Amount, c.Currency, fmtTime(c.AsOf)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListAzureCosts(period string) ([]AzureCost, error) {
	rows, err := s.db.Query(`SELECT resource_id, period, amount, currency, as_of
		FROM azure_costs WHERE period = ? ORDER BY amount DESC, resource_id`, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureCost
	for rows.Next() {
		var c AzureCost
		var asOf string
		if err := rows.Scan(&c.ResourceID, &c.Period, &c.Amount, &c.Currency, &asOf); err != nil {
			return nil, err
		}
		if c.AsOf, err = parseTime(asOf); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetAzureSync(a AzureSync) error {
	ok := 0
	if a.OK {
		ok = 1
	}
	_, err := s.db.Exec(`INSERT INTO azure_sync(scope, ok, message, started_at, ended_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(scope) DO UPDATE SET ok=excluded.ok, message=excluded.message,
		  started_at=excluded.started_at, ended_at=excluded.ended_at`,
		a.Scope, ok, a.Message, fmtTime(a.StartedAt), fmtTime(a.EndedAt))
	return err
}

func (s *Store) AzureSyncState() (map[string]AzureSync, error) {
	rows, err := s.db.Query(`SELECT scope, ok, message, started_at, ended_at FROM azure_sync`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AzureSync{}
	for rows.Next() {
		var a AzureSync
		var ok int
		var started, ended string
		if err := rows.Scan(&a.Scope, &ok, &a.Message, &started, &ended); err != nil {
			return nil, err
		}
		a.OK = ok == 1
		if a.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if a.EndedAt, err = parseTime(ended); err != nil {
			return nil, err
		}
		out[a.Scope] = a
	}
	return out, rows.Err()
}

// PurgeAzure drops everything Azure. Used when the configured scope changes, so
// resources from a group nobody watches any more stop being displayed — they
// would otherwise linger forever, never swept because never synced.
func (s *Store) PurgeAzure() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"azure_resources", "azure_costs", "azure_sync"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BreakAzureSyncForTests drops the azure_sync table so a test can prove that a
// failed read of it surfaces instead of being swallowed into an empty map.
// Narrow on purpose: an ExecForTests taking arbitrary SQL would ship arbitrary
// statement execution in the production binary. Closing the whole store would
// not do — an earlier read would fail first and prove nothing about this one.
func (s *Store) BreakAzureSyncForTests() error {
	_, err := s.db.Exec("DROP TABLE azure_sync")
	return err
}
