package store

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func TestCreateHostReturnsTokenOnceAndStoresHash(t *testing.T) {
	s := openTest(t)
	h, tok, err := s.CreateHost("pi", t0)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID == 0 || h.Name != "pi" || h.Status != "never_seen" || tok == "" {
		t.Fatalf("host = %+v tok=%q", h, tok)
	}
	var stored string
	s.db.QueryRow(`SELECT token_hash FROM hosts WHERE id = ?`, h.ID).Scan(&stored)
	if stored == tok || stored != HashToken(tok) {
		t.Fatalf("token must be stored hashed: %q", stored)
	}
	got, err := s.HostByToken(tok)
	if err != nil || got.ID != h.ID {
		t.Fatalf("HostByToken = %+v %v", got, err)
	}
}

func TestHostByTokenUnknown(t *testing.T) {
	s := openTest(t)
	if _, err := s.HostByToken("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRegenerateTokenInvalidatesOld(t *testing.T) {
	s := openTest(t)
	h, old, _ := s.CreateHost("pi", t0)
	fresh, err := s.RegenerateToken(h.ID)
	if err != nil || fresh == old {
		t.Fatalf("regenerate = %q %v", fresh, err)
	}
	if _, err := s.HostByToken(old); !errors.Is(err, ErrNotFound) {
		t.Fatal("old token must be rejected")
	}
	if _, err := s.HostByToken(fresh); err != nil {
		t.Fatal("fresh token must work")
	}
}

func TestCreateHostDuplicateName(t *testing.T) {
	s := openTest(t)
	s.CreateHost("pi", t0)
	if _, _, err := s.CreateHost("pi", t0); err == nil {
		t.Fatal("duplicate name must fail")
	}
}

func TestUpdateInfoStatusAndList(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("zed", t0)
	s.CreateHost("alpha", t0)
	if err := s.UpdateHostInfo(h.ID, HostInfo{OS: "linux", Arch: "arm64", Hostname: "zed.local", AgentVersion: "0.1", Cores: 4, MemTotal: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	seen := t0.Add(time.Minute)
	if err := s.SetHostStatus(h.ID, "online", &seen); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Host(h.ID)
	if got.Status != "online" || got.LastSeen == nil || !got.LastSeen.Equal(seen) || got.Cores != 4 || got.OS != "linux" {
		t.Fatalf("host = %+v", got)
	}
	list, _ := s.ListHosts()
	if len(list) != 2 || list[0].Name != "alpha" {
		t.Fatalf("list = %+v", list)
	}
}

func TestMarkStale(t *testing.T) {
	s := openTest(t)
	a, _, _ := s.CreateHost("a", t0)
	b, _, _ := s.CreateHost("b", t0)
	c, _, _ := s.CreateHost("c", t0) // never seen: must stay never_seen
	old := t0
	recent := t0.Add(50 * time.Second)
	s.SetHostStatus(a.ID, "online", &old)
	s.SetHostStatus(b.ID, "online", &recent)
	now := t0.Add(time.Minute)
	stale, err := s.MarkStale(now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != a.ID {
		t.Fatalf("stale = %+v", stale)
	}
	ga, _ := s.Host(a.ID)
	gb, _ := s.Host(b.ID)
	gc, _ := s.Host(c.ID)
	if ga.Status != "offline" || gb.Status != "online" || gc.Status != "never_seen" {
		t.Fatalf("statuses = %s %s %s", ga.Status, gb.Status, gc.Status)
	}
	if again, _ := s.MarkStale(now, 30*time.Second); len(again) != 0 {
		t.Fatalf("second MarkStale = %+v", again)
	}
}

func TestDeleteHostCascades(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	at := fmtTime(t0)
	s.db.Exec(`INSERT INTO samples(host_id, at, cpu) VALUES (?, ?, 1)`, h.ID, at)
	s.db.Exec(`INSERT INTO samples_10m(host_id, at) VALUES (?, ?)`, h.ID, at)
	s.db.Exec(`INSERT INTO samples_1h(host_id, at) VALUES (?, ?)`, h.ID, at)
	s.db.Exec(`INSERT INTO samples_1d(host_id, at) VALUES (?, ?)`, h.ID, at)
	s.db.Exec(`INSERT INTO containers(host_id, name, updated_at) VALUES (?, 'c', ?)`, h.ID, at)
	s.db.Exec(`INSERT INTO container_samples(host_id, name, at) VALUES (?, 'c', ?)`, h.ID, at)
	s.db.Exec(`INSERT INTO alert_events(rule_id, host_id, metric, kind, value, at) VALUES (1, ?, 'cpu', 'fired', 1, ?)`, h.ID, at)
	children := []string{"samples", "samples_10m", "samples_1h", "samples_1d", "containers", "container_samples", "alert_events"}
	// Prove the fixtures landed: otherwise this test would pass on an empty database.
	for _, table := range children {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("fixture for %s did not land (n=%d err=%v)", table, n, err)
		}
	}
	if err := s.DeleteHost(h.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range children {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s not cascaded: %d rows left", table, n)
		}
	}
	if _, err := s.Host(h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("host still there")
	}
}
