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

func TestRolesAndTheLastLocalAdmin(t *testing.T) {
	s := openTest(t)
	admin, _ := s.CreateUser("v@example.com", "hash", t0)
	if admin.Role != RoleAdmin || admin.Provider != ProviderLocal {
		t.Fatalf("the setup account is a local admin: %+v", admin)
	}
	ann, err := s.AddUser("Ann@Example.com", "", RoleViewer, ProviderEntra, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("ann@example.com", "", RoleAdmin, ProviderEntra, t0); !errors.Is(err, ErrUserExists) {
		t.Fatalf("a second ann = %v", err)
	}
	got, _ := s.UserByEmail("ann@example.com")
	if got.Role != RoleViewer || got.Provider != ProviderEntra || got.PasswordHash != "" {
		t.Fatalf("ann = %+v", got)
	}

	// The only local admin can be neither demoted nor removed, even when an
	// Entra admin exists: the local account is the way in when Entra is down.
	s.SetUserRole(ann.ID, RoleAdmin)
	if err := s.SetUserRole(admin.ID, RoleViewer); !errors.Is(err, ErrLastLocalAdmin) {
		t.Fatalf("demote = %v", err)
	}
	if err := s.DeleteUser(admin.ID); !errors.Is(err, ErrLastLocalAdmin) {
		t.Fatalf("delete = %v", err)
	}

	// Removing someone signs them out.
	tok, _ := s.CreateSession(ann.ID, t0.Add(time.Hour))
	if err := s.DeleteUser(ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(tok, t0); !errors.Is(err, ErrNotFound) {
		t.Fatal("a removed user's session must stop working")
	}
	if err := s.SetUserRole(ann.ID, RoleAdmin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("role of a removed user = %v", err)
	}
	if users, _ := s.ListUsers(); len(users) != 1 {
		t.Fatalf("users = %+v", users)
	}
}
