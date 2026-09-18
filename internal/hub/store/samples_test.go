package store

import (
	"errors"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

func f(v float64) *float64 { return &v }
func i(v int64) *int64     { return &v }

func sampleAt(at time.Time, cpu float64) *proto.Sample {
	return &proto.Sample{Type: proto.TypeSample, At: at, CPU: f(cpu), MemUsed: i(100), MemTotal: i(200),
		Disks: []proto.Disk{{Mount: "/", Used: 10, Total: 100}}, Net: &proto.Net{SentBps: 1, RecvBps: 2}}
}

func TestInsertAndLatest(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	if err := s.InsertSample(h.ID, sampleAt(t0, 50)); err != nil {
		t.Fatal(err)
	}
	row, err := s.LatestSample(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *row.CPU != 50 || *row.MemUsed != 100 || row.Load1 != nil || row.Uptime != nil {
		t.Fatalf("row = %+v (missing metrics must be nil)", row)
	}
	if len(row.Disks) != 1 || row.Disks[0].Mount != "/" || *row.NetSentBps != 1 {
		t.Fatalf("row = %+v", row)
	}
}

func TestInsertRejectsOlderThanLast(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	s.InsertSample(h.ID, sampleAt(t0.Add(10*time.Second), 1))
	if err := s.InsertSample(h.ID, sampleAt(t0, 2)); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if err := s.InsertSample(h.ID, sampleAt(t0.Add(10*time.Second), 3)); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale for duplicate, got %v", err)
	}
}

func TestLatestNotFound(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	if _, err := s.LatestSample(h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, ok, _ := s.LastSampleAt(h.ID); ok {
		t.Fatal("LastSampleAt must report absence")
	}
}

func TestTableForPeriod(t *testing.T) {
	cases := map[string]string{"1h": "samples", "24h": "samples", "7d": "samples_10m", "30d": "samples_1h", "1y": "samples_1d"}
	for p, want := range cases {
		got, span, err := TableForPeriod(p)
		if err != nil || got != want || span <= 0 {
			t.Fatalf("%s -> %s %v %v", p, got, span, err)
		}
	}
	if _, _, err := TableForPeriod("2w"); err == nil {
		t.Fatal("unknown period must error")
	}
}

func TestSeriesRawWindow(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	now := t0.Add(2 * time.Hour)
	s.InsertSample(h.ID, sampleAt(now.Add(-90*time.Minute), 10))
	s.InsertSample(h.ID, sampleAt(now.Add(-30*time.Minute), 20))
	s.InsertSample(h.ID, sampleAt(now.Add(-10*time.Minute), 30))
	pts, err := s.Series(h.ID, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || *pts[0].CPU != 20 || *pts[1].CPU != 30 || pts[0].CPUMax != nil {
		t.Fatalf("pts = %+v", pts)
	}
}

func TestSeriesKeepsNullAsNull(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	sm := sampleAt(t0, 1)
	sm.CPU = nil
	s.InsertSample(h.ID, sm)
	pts, _ := s.Series(h.ID, "1h", t0.Add(time.Minute))
	if len(pts) != 1 || pts[0].CPU != nil {
		t.Fatalf("a metric that was not collected must stay nil: %+v", pts)
	}
}

func TestContainersUpsert(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	sm := sampleAt(t0, 1)
	sm.DockerAvailable = true
	sm.Containers = []proto.Container{{Name: "web", Image: "nginx", Status: "running", CPU: 3, MemUsed: 50}}
	s.InsertSample(h.ID, sm)
	sm2 := sampleAt(t0.Add(10*time.Second), 1)
	sm2.DockerAvailable = true
	sm2.Containers = []proto.Container{{Name: "web", Image: "nginx", Status: "running", CPU: 5, MemUsed: 60}}
	s.InsertSample(h.ID, sm2)
	cs, err := s.Containers(h.ID)
	if err != nil || len(cs) != 1 || *cs[0].CPU != 5 || *cs[0].MemUsed != 60 {
		t.Fatalf("containers = %+v %v", cs, err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM container_samples`).Scan(&n)
	if n != 2 {
		t.Fatalf("container_samples = %d", n)
	}
}

func TestContainersRemovedFromCollectedListAreDeleted(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	sm := sampleAt(t0, 1)
	sm.DockerAvailable = true
	sm.Containers = []proto.Container{{Name: "web"}, {Name: "db"}}
	s.InsertSample(h.ID, sm)
	sm2 := sampleAt(t0.Add(10*time.Second), 1)
	sm2.DockerAvailable = true
	sm2.Containers = []proto.Container{{Name: "web"}}
	s.InsertSample(h.ID, sm2)
	cs, _ := s.Containers(h.ID)
	if len(cs) != 1 || cs[0].Name != "web" {
		t.Fatalf("a container gone from a collected list must be removed: %+v", cs)
	}
}

func TestContainersUnavailableKeepsLastKnown(t *testing.T) {
	s := openTest(t)
	h, _, _ := s.CreateHost("a", t0)
	sm := sampleAt(t0, 1)
	sm.DockerAvailable = true
	sm.Containers = []proto.Container{{Name: "web"}}
	s.InsertSample(h.ID, sm)
	sm2 := sampleAt(t0.Add(10*time.Second), 1) // DockerAvailable=false: not collected
	s.InsertSample(h.ID, sm2)
	cs, _ := s.Containers(h.ID)
	if len(cs) != 1 {
		t.Fatalf("unavailable docker must not wipe containers: %+v", cs)
	}
}
