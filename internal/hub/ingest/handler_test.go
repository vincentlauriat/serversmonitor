package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

type fakeNotifier struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeNotifier) Publish(typ string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, typ)
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

type rig struct {
	st  *store.Store
	srv *httptest.Server
	n   *fakeNotifier
	h   *Handler
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{}
	cfg := DefaultConfig()
	cfg.MinSampleGap = 0
	h := New(st, n, cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &rig{st: st, srv: srv, n: n, h: h}
}

func (r *rig) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(r.srv.URL, "http")
	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
}

func send(t *testing.T, c *websocket.Conn, msg any) {
	t.Helper()
	data, err := proto.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c *websocket.Conn) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := proto.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func hello() *proto.Hello {
	return &proto.Hello{Type: proto.TypeHello, AgentVersion: "0.1", OS: "linux", Arch: "arm64", Hostname: "pi", Cores: 4, MemTotal: 8 << 30}
}

func handshake(t *testing.T, r *rig, token string) *websocket.Conn {
	t.Helper()
	c, _, err := r.dial(t, token)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello())
	w, ok := recv(t, c).(*proto.Welcome)
	if !ok || w.IntervalSec != 10 {
		t.Fatalf("want welcome with interval 10, got %+v", w)
	}
	return c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestRejectsBadToken(t *testing.T) {
	r := newRig(t)
	_, resp, err := r.dial(t, "nope")
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got err=%v resp=%v", err, resp)
	}
}

func TestRejectsMissingToken(t *testing.T) {
	r := newRig(t)
	resp, err := http.Get(r.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestHelloWelcomeAndSample(t *testing.T) {
	r := newRig(t)
	h, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	defer c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { got, _ := r.st.Host(h.ID); return got.Status == "online" && got.Cores == 4 })
	cpu := 42.0
	send(t, c, &proto.Sample{Type: proto.TypeSample, At: time.Now().UTC(), CPU: &cpu})
	waitFor(t, func() bool { _, err := r.st.LatestSample(h.ID); return err == nil })
	row, _ := r.st.LatestSample(h.ID)
	if *row.CPU != 42 || row.MemUsed != nil {
		t.Fatalf("row = %+v (uncollected metrics must stay nil)", row)
	}
	if r.n.count() < 2 {
		t.Fatalf("expected host events for hello and sample, got %d", r.n.count())
	}
}

func TestFirstMessageMustBeHello(t *testing.T) {
	r := newRig(t)
	_, tok, _ := r.st.CreateHost("pi", time.Now())
	c, _, err := r.dial(t, tok)
	if err != nil {
		t.Fatal(err)
	}
	cpu := 1.0
	send(t, c, &proto.Sample{Type: proto.TypeSample, At: time.Now(), CPU: &cpu})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("hub must close the connection")
	}
}

func TestSecondConnectionReplacesFirst(t *testing.T) {
	r := newRig(t)
	_, tok, _ := r.st.CreateHost("pi", time.Now())
	c1 := handshake(t, r, tok)
	c2 := handshake(t, r, tok)
	defer c2.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c1.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Fatalf("first socket must be closed with policy violation, got %v", err)
	}
	waitFor(t, func() bool { return len(r.h.Connected()) == 1 })
}

func TestStaleSampleIsDroppedNotFatal(t *testing.T) {
	r := newRig(t)
	h, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	defer c.Close(websocket.StatusNormalClosure, "")
	now := time.Now().UTC()
	cpu := 1.0
	send(t, c, &proto.Sample{Type: proto.TypeSample, At: now, CPU: &cpu})
	waitFor(t, func() bool { _, err := r.st.LatestSample(h.ID); return err == nil })
	old := 99.0
	send(t, c, &proto.Sample{Type: proto.TypeSample, At: now.Add(-time.Minute), CPU: &old})
	fresh := 2.0
	send(t, c, &proto.Sample{Type: proto.TypeSample, At: now.Add(time.Second), CPU: &fresh})
	waitFor(t, func() bool { row, _ := r.st.LatestSample(h.ID); return row.CPU != nil && *row.CPU == 2 })
}

func TestUnknownFieldIsIgnoredNotFatal(t *testing.T) {
	r := newRig(t)
	h, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	defer c.Close(websocket.StatusNormalClosure, "")
	at := time.Now().UTC().Format(time.RFC3339Nano)
	raw := []byte(`{"v":1,"type":"sample","at":"` + at + `","cpu":7,"quantum_flux":42}`)
	if err := c.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { row, err := r.st.LatestSample(h.ID); return err == nil && row.CPU != nil && *row.CPU == 7 })
}

func TestOversizedMessageCloses(t *testing.T) {
	r := newRig(t)
	_, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	big := strings.Repeat("x", 300<<10)
	c.Write(context.Background(), websocket.MessageText, []byte(`{"v":1,"type":"sample","at":"2026-01-01T00:00:00Z","pad":"`+big+`"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("oversized message must close the socket")
	}
}

func TestMalformedMessageCloses(t *testing.T) {
	r := newRig(t)
	_, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	c.Write(context.Background(), websocket.MessageText, []byte(`{not json`))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("malformed message must close the socket")
	}
}

func TestReconfigureReachesAgent(t *testing.T) {
	r := newRig(t)
	_, tok, _ := r.st.CreateHost("pi", time.Now())
	c := handshake(t, r, tok)
	defer c.Close(websocket.StatusNormalClosure, "")
	r.h.Reconfigure(30)
	if m, ok := recv(t, c).(*proto.Reconfigure); !ok || m.IntervalSec != 30 {
		t.Fatalf("got %+v", m)
	}
}
