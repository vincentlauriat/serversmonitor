package store

import (
	"errors"
	"testing"
	"time"
)

func TestUsersAndSessions(t *testing.T) {
	s := openTest(t)
	if n, _ := s.CountUsers(); n != 0 {
		t.Fatal("fresh db must have no user")
	}
	u, err := s.CreateUser("v@example.com", "hash", t0)
	if err != nil || u.ID == 0 {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("v@example.com", "x", t0); err == nil {
		t.Fatal("duplicate email must fail")
	}
	got, _ := s.UserByEmail("V@EXAMPLE.COM")
	if got.ID != u.ID {
		t.Fatal("email lookup must be case-insensitive")
	}
	tok, err := s.CreateSession(u.ID, t0.Add(time.Hour))
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if su, err := s.SessionUser(tok, t0.Add(30*time.Minute)); err != nil || su.ID != u.ID {
		t.Fatalf("session user = %+v %v", su, err)
	}
	if _, err := s.SessionUser(tok, t0.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session must be rejected")
	}
	s.UpdatePassword(u.ID, "hash2")
	if got, _ := s.UserByEmail("v@example.com"); got.PasswordHash != "hash2" {
		t.Fatal("password not updated")
	}
	s.DeleteSession(tok)
	if _, err := s.SessionUser(tok, t0); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session must be rejected")
	}
	s.CreateSession(u.ID, t0.Add(-time.Minute))
	s.PurgeSessions(t0)
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Fatalf("purge left %d sessions", n)
	}
}
