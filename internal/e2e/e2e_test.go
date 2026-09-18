package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/agent/collect"
	"github.com/vincentlauriat/serversmonitor/internal/agent/link"
	"github.com/vincentlauriat/serversmonitor/internal/hub"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

// stubSys is a machine whose numbers are known, so the assertions can be exact.
type stubSys struct{}

func (stubSys) CPUPercent(context.Context) (float64, error) { return 33, nil }
func (stubSys) Memory(context.Context) (int64, int64, int64, int64, error) {
	return 1, 2, 0, 0, nil
}
func (stubSys) Load(context.Context) (float64, float64, float64, error) { return 0.1, 0.2, 0.3, nil }
func (stubSys) Uptime(context.Context) (int64, error)                   { return 10, nil }
func (stubSys) Disks(context.Context) ([]collect.DiskUsage, error) {
	return []collect.DiskUsage{{Mount: "/", Used: 1, Total: 4}}, nil
}
func (stubSys) Net(context.Context) (int64, int64, error)   { return 0, 0, nil }
func (stubSys) Temps(context.Context) ([]proto.Temp, error) { return nil, collect.ErrUnavailable }

func TestSampleTravelsFromAgentToAPI(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), LogLevel: slog.LevelWarn}
	h, err := hub.New(cfg, "e2e", slog.New(slog.NewTextHandler(discard{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	post := func(path string, body any) *http.Response {
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		resp, err := client.Post(srv.URL+path, "application/json", &buf)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("/api/v1/setup", map[string]string{"email": "v@example.com", "password": "hunter22"}); resp.StatusCode != 201 {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	resp := post("/api/v1/hosts", map[string]string{"name": "e2e"})
	var created struct {
		Host struct {
			ID int64 `json:"id"`
		} `json:"host"`
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	collector := collect.New(stubSys{}, nil, nil, slog.New(slog.NewTextHandler(discard{}, nil)))
	go link.Run(ctx, link.Config{HubURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Token: created.Token, Insecure: true,
		Hello:     proto.Hello{AgentVersion: "e2e", OS: "test", Arch: "test", Hostname: "e2e", Cores: 2, MemTotal: 2},
		OnWelcome: func(w *proto.Welcome) { collector.SetIgnoreMounts(w.IgnoreMounts) },
		Log:       slog.New(slog.NewTextHandler(discard{}, nil))}, collector.Collect)

	deadline := time.Now().Add(25 * time.Second) // the default interval is 10 s
	for time.Now().Before(deadline) {
		resp, err := client.Get(srv.URL + "/api/v1/hosts")
		if err != nil {
			t.Fatal(err)
		}
		var hosts []struct {
			Status       string `json:"status"`
			Cores        int    `json:"cores"`
			AgentVersion string `json:"agent_version"`
			Latest       *struct {
				CPU   *float64 `json:"cpu"`
				Temps []any    `json:"temps"`
				Disks []struct {
					Mount string `json:"mount"`
				} `json:"disks"`
			} `json:"latest"`
		}
		json.NewDecoder(resp.Body).Decode(&hosts)
		resp.Body.Close()
		if len(hosts) == 1 && hosts[0].Status == "online" && hosts[0].Latest != nil && hosts[0].Latest.CPU != nil {
			if *hosts[0].Latest.CPU != 33 {
				t.Fatalf("cpu = %v", *hosts[0].Latest.CPU)
			}
			if hosts[0].Latest.Temps != nil {
				t.Fatal("temps were not collected and must come back as null, not an empty list")
			}
			if len(hosts[0].Latest.Disks) != 1 || hosts[0].Latest.Disks[0].Mount != "/" {
				t.Fatalf("disks = %+v", hosts[0].Latest.Disks)
			}
			if hosts[0].Cores != 2 || hosts[0].AgentVersion != "e2e" {
				t.Fatalf("hello did not reach the API: %+v", hosts[0])
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("sample never reached the API")
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
