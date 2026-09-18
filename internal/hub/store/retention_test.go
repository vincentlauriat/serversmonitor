package store

import (
	"testing"
	"time"
)

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAggregateBuildsTenMinuteWindows(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	base := t0.Truncate(10 * time.Minute)
	for k := 0; k < 6; k++ {
		s.InsertSample(h.ID, sampleAt(base.Add(time.Duration(k)*90*time.Second), float64((k+1)*10)))
	}
	s.InsertSample(h.ID, sampleAt(base.Add(10*time.Minute+30*time.Second), 99))
	now := base.Add(12 * time.Minute)
	if err := s.Aggregate(now); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "samples_10m"); n != 1 {
		t.Fatalf("samples_10m rows = %d, want 1 (window in progress must not be written)", n)
	}
	var avg, min, max float64
	var at string
	s.db.QueryRow(`SELECT at, cpu_avg, cpu_min, cpu_max FROM samples_10m`).Scan(&at, &avg, &min, &max)
	if at != fmtTime(base) || avg != 35 || min != 10 || max != 60 {
		t.Fatalf("window = %s avg=%v min=%v max=%v", at, avg, min, max)
	}
}

func TestAggregateIsIdempotent(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	base := t0.Truncate(time.Hour)
	for k := 0; k < 30; k++ {
		s.InsertSample(h.ID, sampleAt(base.Add(time.Duration(k)*2*time.Minute), float64(k)))
	}
	now := base.Add(2 * time.Hour)
	if err := s.Aggregate(now); err != nil {
		t.Fatal(err)
	}
	n10, n1h := count(t, s, "samples_10m"), count(t, s, "samples_1h")
	if n10 != 6 || n1h != 1 {
		t.Fatalf("after first run: 10m=%d 1h=%d", n10, n1h)
	}
	var avgBefore float64
	s.db.QueryRow(`SELECT cpu_avg FROM samples_1h`).Scan(&avgBefore)
	if err := s.Aggregate(now); err != nil {
		t.Fatal(err)
	}
	var avgAfter float64
	s.db.QueryRow(`SELECT cpu_avg FROM samples_1h`).Scan(&avgAfter)
	if count(t, s, "samples_10m") != n10 || count(t, s, "samples_1h") != n1h || avgBefore != avgAfter {
		t.Fatalf("second run changed the result: rows %d/%d, avg %v -> %v", count(t, s, "samples_10m"), count(t, s, "samples_1h"), avgBefore, avgAfter)
	}
}

func TestAggregateCarriesDisksAndTemps(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	base := t0.Truncate(10 * time.Minute)
	s.InsertSample(h.ID, sampleAt(base, 1))
	s.Aggregate(base.Add(11 * time.Minute))
	var disks *string
	s.db.QueryRow(`SELECT disks FROM samples_10m`).Scan(&disks)
	if disks == nil || *disks == "" {
		t.Fatal("the window must carry the last disks JSON")
	}
}

func TestAggregateNullStaysNull(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	base := t0.Truncate(10 * time.Minute)
	sm := sampleAt(base, 1)
	sm.CPU = nil // not collected
	s.InsertSample(h.ID, sm)
	s.Aggregate(base.Add(11 * time.Minute))
	var cpu *float64
	s.db.QueryRow(`SELECT cpu_avg FROM samples_10m`).Scan(&cpu)
	if cpu != nil {
		t.Fatalf("cpu_avg must be NULL when nothing was collected, got %v", *cpu)
	}
}

func TestPurge(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	now := t0.Add(48 * time.Hour)
	s.InsertSample(h.ID, sampleAt(now.Add(-30*time.Hour), 1))
	s.InsertSample(h.ID, sampleAt(now.Add(-1*time.Hour), 2))
	s.db.Exec(`INSERT INTO samples_10m(host_id, at) VALUES (?, ?)`, h.ID, fmtTime(now.Add(-40*24*time.Hour)))
	s.db.Exec(`INSERT INTO samples_10m(host_id, at) VALUES (?, ?)`, h.ID, fmtTime(now.Add(-2*24*time.Hour)))
	s.db.Exec(`INSERT INTO container_samples(host_id, name, at) VALUES (?, 'c', ?)`, h.ID, fmtTime(now.Add(-30*time.Hour)))
	r := Retention{Raw: 24 * time.Hour, TenMin: 30 * 24 * time.Hour, Hour: 365 * 24 * time.Hour}
	if err := s.Purge(now, r); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "samples") != 1 || count(t, s, "samples_10m") != 1 || count(t, s, "container_samples") != 0 {
		t.Fatalf("purge left samples=%d 10m=%d cs=%d", count(t, s, "samples"), count(t, s, "samples_10m"), count(t, s, "container_samples"))
	}
}

func TestRetentionFromSettings(t *testing.T) {
	s := openTest(t)
	s.SetSetting("retention_raw_hours", "48")
	r := s.RetentionFromSettings()
	if r.Raw != 48*time.Hour || r.TenMin != 30*24*time.Hour || r.Hour != 365*24*time.Hour {
		t.Fatalf("retention = %+v", r)
	}
}
