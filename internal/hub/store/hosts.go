package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

type Host struct {
	ID           int64
	Name         string
	Status       string
	LastSeen     *time.Time
	OS           string
	Arch         string
	Hostname     string
	AgentVersion string
	Cores        int
	MemTotal     int64
	Muted        bool
	CreatedAt    time.Time
}

type HostInfo struct {
	OS, Arch, Hostname, AgentVersion string
	Cores                            int
	MemTotal                         int64
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const hostCols = `id, name, status, last_seen, os, arch, hostname, agent_version, cores, mem_total, muted, created_at`

type scanner interface{ Scan(...any) error }

func scanHost(row scanner) (Host, error) {
	var h Host
	var lastSeen sql.NullString
	var created string
	var muted int
	err := row.Scan(&h.ID, &h.Name, &h.Status, &lastSeen, &h.OS, &h.Arch, &h.Hostname, &h.AgentVersion, &h.Cores, &h.MemTotal, &muted, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	h.Muted = muted == 1
	if lastSeen.Valid {
		t, err := parseTime(lastSeen.String)
		if err != nil {
			return h, err
		}
		h.LastSeen = &t
	}
	h.CreatedAt, err = parseTime(created)
	return h, err
}

func (s *Store) CreateHost(name string, now time.Time) (Host, string, error) {
	tok, err := newToken()
	if err != nil {
		return Host{}, "", err
	}
	res, err := s.db.Exec(`INSERT INTO hosts(name, token_hash, created_at) VALUES (?, ?, ?)`, name, HashToken(tok), fmtTime(now))
	if err != nil {
		return Host{}, "", err
	}
	id, _ := res.LastInsertId()
	h, err := s.Host(id)
	return h, tok, err
}

func (s *Store) RegenerateToken(id int64) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	res, err := s.db.Exec(`UPDATE hosts SET token_hash = ? WHERE id = ?`, HashToken(tok), id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return tok, nil
}

func (s *Store) HostByToken(token string) (Host, error) {
	return scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE token_hash = ?`, HashToken(token)))
}

func (s *Store) Host(id int64) (Host, error) {
	return scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE id = ?`, id))
}

func (s *Store) ListHosts() ([]Host, error) {
	rows, err := s.db.Query(`SELECT ` + hostCols + ` FROM hosts ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) execOne(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RenameHost(id int64, name string) error {
	return s.execOne(`UPDATE hosts SET name = ? WHERE id = ?`, name, id)
}

func (s *Store) DeleteHost(id int64) error {
	return s.execOne(`DELETE FROM hosts WHERE id = ?`, id)
}

func (s *Store) SetMuted(id int64, muted bool) error {
	m := 0
	if muted {
		m = 1
	}
	return s.execOne(`UPDATE hosts SET muted = ? WHERE id = ?`, m, id)
}

func (s *Store) UpdateHostInfo(id int64, info HostInfo) error {
	return s.execOne(`UPDATE hosts SET os=?, arch=?, hostname=?, agent_version=?, cores=?, mem_total=? WHERE id = ?`,
		info.OS, info.Arch, info.Hostname, info.AgentVersion, info.Cores, info.MemTotal, id)
}

func (s *Store) SetHostStatus(id int64, status string, seen *time.Time) error {
	if seen == nil {
		return s.execOne(`UPDATE hosts SET status = ? WHERE id = ?`, status, id)
	}
	return s.execOne(`UPDATE hosts SET status = ?, last_seen = ? WHERE id = ?`, status, fmtTime(*seen), id)
}

// MarkStale flips online hosts not seen since now-maxAge to offline and returns them.
func (s *Store) MarkStale(now time.Time, maxAge time.Duration) ([]Host, error) {
	cutoff := fmtTime(now.Add(-maxAge))
	rows, err := s.db.Query(`SELECT `+hostCols+` FROM hosts WHERE status = 'online' AND last_seen < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	var stale []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		stale = append(stale, h)
	}
	rows.Close()
	for i := range stale {
		if err := s.SetHostStatus(stale[i].ID, "offline", nil); err != nil {
			return nil, err
		}
		stale[i].Status = "offline"
	}
	return stale, nil
}
