package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(email, hash string, now time.Time) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	res, err := s.db.Exec(`INSERT INTO users(email, password_hash, created_at) VALUES (?,?,?)`, email, hash, fmtTime(now))
	if err != nil {
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: hash, CreatedAt: now}, nil
}

func (s *Store) scanUser(row scanner) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &created)
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
	return s.scanUser(s.db.QueryRow(`SELECT id, email, password_hash, created_at FROM users WHERE email = ?`, email))
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
	return s.scanUser(s.db.QueryRow(`SELECT u.id, u.email, u.password_hash, u.created_at FROM sessions se JOIN users u ON u.id = se.user_id
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
