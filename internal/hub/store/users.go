package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Roles and providers. An admin changes anything; a viewer sees everything
// and changes nothing. A local account signs in with a password; an Entra
// account only through Microsoft Entra ID, and has no password at all.
const (
	RoleAdmin     = "admin"
	RoleViewer    = "viewer"
	ProviderLocal = "local"
	ProviderEntra = "entra"
)

var (
	ErrUserExists     = errors.New("store: a user with this e-mail already exists")
	ErrLastLocalAdmin = errors.New("store: the last local admin cannot be removed or demoted")
)

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Role         string
	Provider     string
	CreatedAt    time.Time
}

const userCols = `id, email, password_hash, role, provider, created_at`

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser is the local admin made at setup: a password, every right.
func (s *Store) CreateUser(email, hash string, now time.Time) (User, error) {
	return s.AddUser(email, hash, RoleAdmin, ProviderLocal, now)
}

// AddUser writes an account. An Entra account is added by an admin before
// its owner ever signs in: the row is the allow list.
func (s *Store) AddUser(email, hash, role, provider string, now time.Time) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	res, err := s.db.Exec(`INSERT INTO users(email, password_hash, role, provider, created_at) VALUES (?,?,?,?,?)`,
		email, hash, role, provider, fmtTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: hash, Role: role, Provider: provider, CreatedAt: now}, nil
}

func (s *Store) scanUser(row scanner) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Provider, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.CreatedAt, err = parseTime(created)
	return u, err
}

func (s *Store) UserByEmail(email string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// guardLastLocalAdmin refuses a change that would leave no local admin. The
// local account is the way back in when Entra is misconfigured or down, so
// it must survive every edit an admin can make from the page.
func (s *Store) guardLastLocalAdmin(tx *sql.Tx, id int64) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM users WHERE role = ? AND provider = ? AND id != ?`,
		RoleAdmin, ProviderLocal, id).Scan(&n); err != nil {
		return err
	}
	var role, provider string
	err := tx.QueryRow(`SELECT role, provider FROM users WHERE id = ?`, id).Scan(&role, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == RoleAdmin && provider == ProviderLocal && n == 0 {
		return ErrLastLocalAdmin
	}
	return nil
}

// SetUserRole changes a role. Demoting the last local admin is refused.
func (s *Store) SetUserRole(id int64, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role != RoleAdmin {
		if err := s.guardLastLocalAdmin(tx, id); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DeleteUser removes an account and, by cascade, its sessions: a removed
// person is signed out at once. Removing the last local admin is refused.
func (s *Store) DeleteUser(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.guardLastLocalAdmin(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdatePassword(userID int64, hash string) error {
	return s.execOne(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
}

func (s *Store) CreateSession(userID int64, expires time.Time) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO sessions(token, user_id, expires_at) VALUES (?,?,?)`, tok, userID, fmtTime(expires))
	return tok, err
}

func (s *Store) SessionUser(token string, now time.Time) (User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT u.id, u.email, u.password_hash, u.role, u.provider, u.created_at
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.token = ? AND se.expires_at > ?`, token, fmtTime(now)))
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (s *Store) PurgeSessions(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, fmtTime(now))
	return err
}
