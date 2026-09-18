package collect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeEngine serves a stub Engine API on a unix socket. The socket lives
// directly in the temp dir with a short name: macOS caps a unix socket path at
// 104 bytes, and t.TempDir() embeds the test name, which blows past it.
func fakeEngine(t *testing.T, listJSON, statsJSON string) string {
	t.Helper()
	f, err := os.CreateTemp("", "smd*.sock")
	if err != nil {
		t.Fatal(err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock) // net.Listen wants to create it itself
	t.Cleanup(func() { os.Remove(sock) })
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(listJSON))
	})
	mux.HandleFunc("/containers/abc/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(statsJSON))
	})
	srv := httptest.NewUnstartedServer(mux)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

const oneContainer = `[{"Id":"abc","Names":["/web"],"Image":"nginx:1","State":"running","Status":"Up 2 hours"}]`
const goodStats = `{"cpu_stats":{"cpu_usage":{"total_usage":200000000},"system_cpu_usage":4000000000,"online_cpus":2},
	"precpu_stats":{"cpu_usage":{"total_usage":100000000},"system_cpu_usage":2000000000},
	"memory_stats":{"usage":104857600,"stats":{"inactive_file":4857600}},
	"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":2000},"eth1":{"rx_bytes":10,"tx_bytes":20}}}`

func TestDockerReaderParsesStats(t *testing.T) {
	d := NewDocker(fakeEngine(t, oneContainer, goodStats))
	stats, err := d.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	c := stats[0]
	// cpu delta 1e8 over system delta 2e9 on 2 cpus = 10 %
	if c.Name != "web" || c.Image != "nginx:1" || c.Status != "running" || c.CPU != 10 || c.MemUsed != 100_000_000 || c.NetSent != 2020 || c.NetRecv != 1010 {
		t.Fatalf("container = %+v", c)
	}
}

func TestDockerReaderSurvivesMissingStats(t *testing.T) {
	// An engine that lists a container but returns junk for its stats must
	// still report the container, with zeroed figures rather than nothing.
	d := NewDocker(fakeEngine(t, oneContainer, "not json"))
	stats, err := d.Containers(context.Background())
	if err != nil || len(stats) != 1 || stats[0].Name != "web" || stats[0].CPU != 0 {
		t.Fatalf("stats = %+v err=%v", stats, err)
	}
}

func TestDockerReaderZeroSystemDeltaGivesZeroCPU(t *testing.T) {
	const flat = `{"cpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000,"online_cpus":2},
		"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},
		"memory_stats":{"usage":50},"networks":{}}`
	d := NewDocker(fakeEngine(t, oneContainer, flat))
	stats, _ := d.Containers(context.Background())
	if len(stats) != 1 || stats[0].CPU != 0 {
		t.Fatalf("a zero system delta must give 0 %%, got %+v", stats)
	}
}

func TestDockerReaderEmptyList(t *testing.T) {
	d := NewDocker(fakeEngine(t, `[]`, goodStats))
	stats, err := d.Containers(context.Background())
	if err != nil || stats == nil || len(stats) != 0 {
		t.Fatalf("an engine with no container is available with an empty list: %+v %v", stats, err)
	}
}

func TestDockerReaderUnavailable(t *testing.T) {
	d := NewDocker(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := d.Containers(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}
