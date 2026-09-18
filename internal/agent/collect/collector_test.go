package collect

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

type fakeSys struct {
	cpu        float64
	cpuErr     error
	memErr     error
	disks      []DiskUsage
	disksErr   error
	sent, recv int64
	netErr     error
	temps      []proto.Temp
	tempsErr   error
}

func (f *fakeSys) CPUPercent(context.Context) (float64, error) { return f.cpu, f.cpuErr }
func (f *fakeSys) Memory(context.Context) (int64, int64, int64, int64, error) {
	return 100, 200, 5, 10, f.memErr
}
func (f *fakeSys) Load(context.Context) (float64, float64, float64, error) { return 1, 2, 3, nil }
func (f *fakeSys) Uptime(context.Context) (int64, error)                   { return 3600, nil }
func (f *fakeSys) Disks(context.Context) ([]DiskUsage, error)              { return f.disks, f.disksErr }
func (f *fakeSys) Net(context.Context) (int64, int64, error)               { return f.sent, f.recv, f.netErr }
func (f *fakeSys) Temps(context.Context) ([]proto.Temp, error)             { return f.temps, f.tempsErr }

type fakeDocker struct {
	stats []ContainerStat
	err   error
}

func (f *fakeDocker) Containers(context.Context) ([]ContainerStat, error) { return f.stats, f.err }

var t0 = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func TestCollectFullSample(t *testing.T) {
	sys := &fakeSys{cpu: 42, disks: []DiskUsage{{Mount: "/", Used: 10, Total: 100, ReadBytes: 1000, WriteBytes: 2000}}, sent: 1000, recv: 2000,
		temps: []proto.Temp{{Sensor: "cpu", Celsius: 55}}}
	c := New(sys, nil, nil, slog.Default())
	s := c.Collect(context.Background(), t0)
	if s.Type != proto.TypeSample || !s.At.Equal(t0) {
		t.Fatalf("envelope = %+v", s)
	}
	if *s.CPU != 42 || *s.MemUsed != 100 || *s.MemTotal != 200 || *s.SwapUsed != 5 || *s.Load5 != 2 || *s.Uptime != 3600 {
		t.Fatalf("scalars = %+v", s)
	}
	if len(s.Disks) != 1 || s.Disks[0].Used != 10 || s.Net == nil || s.Net.SentTotal != 1000 || len(s.Temps) != 1 {
		t.Fatalf("lists = %+v", s)
	}
	if s.DockerAvailable {
		t.Fatal("no docker reader means docker is not available")
	}
}

func TestCollectRatesFromTwoReadings(t *testing.T) {
	sys := &fakeSys{disks: []DiskUsage{{Mount: "/", Used: 1, Total: 2}}, sent: 0, recv: 0}
	c := New(sys, nil, nil, slog.Default())
	first := c.Collect(context.Background(), t0)
	if first.Net.SentBps != 0 || first.Disks[0].ReadBps != 0 {
		t.Fatal("the first reading has no interval to divide by")
	}
	sys.disks[0].ReadBytes, sys.disks[0].WriteBytes = 10_000, 20_000
	sys.sent, sys.recv = 5_000, 15_000
	s := c.Collect(context.Background(), t0.Add(10*time.Second))
	if s.Disks[0].ReadBps != 1000 || s.Disks[0].WriteBps != 2000 {
		t.Fatalf("disk rates = %+v", s.Disks[0])
	}
	if s.Net.SentBps != 500 || s.Net.RecvBps != 1500 {
		t.Fatalf("net rates = %+v", s.Net)
	}
}

func TestCollectCounterResetGivesZeroRate(t *testing.T) {
	sys := &fakeSys{sent: 10_000, recv: 10_000}
	c := New(sys, nil, nil, slog.Default())
	c.Collect(context.Background(), t0)
	sys.sent, sys.recv = 100, 100 // reboot: counters went backwards
	s := c.Collect(context.Background(), t0.Add(10*time.Second))
	if s.Net.SentBps != 0 || s.Net.RecvBps != 0 {
		t.Fatalf("a negative delta must give 0, got %+v", s.Net)
	}
}

func TestCollectFailingReaderLeavesSectionNil(t *testing.T) {
	sys := &fakeSys{cpu: 1, cpuErr: errors.New("no cpu"), memErr: errors.New("no mem"), disksErr: errors.New("x"), netErr: errors.New("x"), tempsErr: errors.New("x")}
	c := New(sys, nil, nil, slog.Default())
	s := c.Collect(context.Background(), t0)
	if s.CPU != nil || s.MemUsed != nil || s.MemTotal != nil || s.Disks != nil || s.Net != nil || s.Temps != nil {
		t.Fatalf("failed sections must be nil: %+v", s)
	}
	if s.Load1 == nil || s.Uptime == nil {
		t.Fatal("working sections must still be collected")
	}
}

func TestCollectWarnsOncePerSection(t *testing.T) {
	sys := &fakeSys{tempsErr: ErrUnavailable}
	var lines int
	h := slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { lines++; return len(p), nil }), nil)
	c := New(sys, nil, nil, slog.New(h))
	for i := 0; i < 5; i++ {
		c.Collect(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if lines != 1 {
		t.Fatalf("a permanently missing sensor must be logged once, got %d lines", lines)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestCollectIgnoresMounts(t *testing.T) {
	sys := &fakeSys{disks: []DiskUsage{{Mount: "/", Total: 1}, {Mount: "/boot/efi", Total: 1}, {Mount: "/snap/x", Total: 1}}}
	c := New(sys, nil, []string{"/boot/efi", "/snap/"}, slog.Default())
	s := c.Collect(context.Background(), t0)
	if len(s.Disks) != 1 || s.Disks[0].Mount != "/" {
		t.Fatalf("disks = %+v", s.Disks)
	}
}

func TestCollectDocker(t *testing.T) {
	d := &fakeDocker{stats: []ContainerStat{{Name: "web", Image: "nginx", Status: "running", CPU: 3, MemUsed: 50}}}
	c := New(&fakeSys{}, d, nil, slog.Default())
	c.Collect(context.Background(), t0)
	d.stats[0].NetSent, d.stats[0].NetRecv = 1000, 2000
	s := c.Collect(context.Background(), t0.Add(10*time.Second))
	if !s.DockerAvailable || len(s.Containers) != 1 || s.Containers[0].CPU != 3 || s.Containers[0].NetSentBps != 100 || s.Containers[0].NetRecvBps != 200 {
		t.Fatalf("containers = %+v avail=%v", s.Containers, s.DockerAvailable)
	}
	d.err = ErrUnavailable
	s = c.Collect(context.Background(), t0.Add(20*time.Second))
	if s.DockerAvailable || s.Containers != nil {
		t.Fatalf("unavailable docker must be reported as such: %+v", s)
	}
}

func TestCollectDockerWithNoContainersIsStillAvailable(t *testing.T) {
	c := New(&fakeSys{}, &fakeDocker{stats: nil}, nil, slog.Default())
	s := c.Collect(context.Background(), t0)
	if !s.DockerAvailable || s.Containers == nil || len(s.Containers) != 0 {
		t.Fatalf("an empty but readable docker is not the same as no docker: avail=%v containers=%v", s.DockerAvailable, s.Containers)
	}
}
