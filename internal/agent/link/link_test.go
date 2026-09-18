package link

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
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

// fakeHub is a minimal hub: it records hellos and samples, and can drop the
// connection with a given index right after welcome.
type fakeHub struct {
	mu        sync.Mutex
	conns     int
	hellos    []*proto.Hello
	samples   []*proto.Sample
	dropAfter int // 1-based; 0 = never
	interval  int
	srv       *httptest.Server
}

func newFakeHub(t *testing.T, interval, dropAfter int) *fakeHub {
	h := &fakeHub{interval: interval, dropAfter: dropAfter}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		h.mu.Lock()
		h.conns++
		n := h.conns
		h.mu.Unlock()
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		msg, _ := proto.Decode(data)
		hello, ok := msg.(*proto.Hello)
		if !ok {
			return
		}
		h.mu.Lock()
		h.hellos = append(h.hellos, hello)
		h.mu.Unlock()
		welcome, _ := proto.Encode(&proto.Welcome{Type: proto.TypeWelcome, IntervalSec: h.interval, IgnoreMounts: []string{"/boot"}})
		c.Write(ctx, websocket.MessageText, welcome)
		if n == h.dropAfter {
			c.Close(websocket.StatusGoingAway, "bye")
			return
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if m, _ := proto.Decode(data); m != nil {
				if s, ok := m.(*proto.Sample); ok {
					h.mu.Lock()
					h.samples = append(h.samples, s)
					h.mu.Unlock()
				}
			}
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) url() string { return "ws" + strings.TrimPrefix(h.srv.URL, "http") }

func (h *fakeHub) count() (int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns, len(h.hellos), len(h.samples)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout")
}

func cfgFor(h *fakeHub) Config {
	return Config{HubURL: h.url(), Token: "tok", Insecure: true,
		Hello:   proto.Hello{Type: proto.TypeHello, AgentVersion: "t", Hostname: "x"},
		Backoff: func(int) time.Duration { return 20 * time.Millisecond },
		Log:     slog.New(slog.NewTextHandler(discard{}, nil))}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func counter() (Sampler, func() int) {
	var mu sync.Mutex
	n := 0
	s := func(ctx context.Context, now time.Time) proto.Sample {
		mu.Lock()
		defer mu.Unlock()
		n++
		v := float64(n)
		return proto.Sample{Type: proto.TypeSample, At: now, CPU: &v}
	}
	return s, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func TestRunSendsHelloThenSamples(t *testing.T) {
	h := newFakeHub(t, 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgFor(h)
	var mu sync.Mutex
	var welcomed *proto.Welcome
	cfg.OnWelcome = func(w *proto.Welcome) { mu.Lock(); welcomed = w; mu.Unlock() }
	sampler, _ := counter()
	go Run(ctx, cfg, sampler)
	waitFor(t, func() bool { _, hellos, samples := h.count(); return hellos == 1 && samples >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if welcomed == nil || len(welcomed.IgnoreMounts) != 1 {
		t.Fatalf("OnWelcome = %+v", welcomed)
	}
}

func TestRunReconnectsAfterDrop(t *testing.T) {
	h := newFakeHub(t, 1, 1) // first connection dropped right after welcome
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sampler, _ := counter()
	go Run(ctx, cfgFor(h), sampler)
	waitFor(t, func() bool { conns, _, samples := h.count(); return conns >= 2 && samples >= 3 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := 1; i < len(h.samples); i++ {
		if !h.samples[i].At.After(h.samples[i-1].At) {
			t.Fatalf("samples out of order: %v then %v", h.samples[i-1].At, h.samples[i].At)
		}
	}
}

// fakeConn is an in-memory Conn: reads come from a channel, writes are recorded.
type fakeConn struct {
	mu     sync.Mutex
	writes [][]byte
	reads  chan []byte
}

func (f *fakeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case d := <-f.reads:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeConn) Write(_ context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	return nil
}

func (f *fakeConn) Close() error { return nil }

func TestSessionReplaysBufferBeforeNewSamples(t *testing.T) {
	fc := &fakeConn{reads: make(chan []byte, 1)}
	welcome, _ := proto.Encode(&proto.Welcome{Type: proto.TypeWelcome, IntervalSec: 1})
	fc.reads <- welcome
	buf := NewBuffer(60)
	buf.Push(proto.Sample{At: time.Unix(100, 0)})
	buf.Push(proto.Sample{At: time.Unix(110, 0)})
	cfg := Config{Dial: func(context.Context, string, string) (Conn, error) { return fc, nil },
		Log: slog.New(slog.NewTextHandler(discard{}, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	sampler, _ := counter()
	session(ctx, cfg, sampler, buf)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var kinds []string
	var ats []int64
	for _, w := range fc.writes {
		m, _ := proto.Decode(w)
		switch x := m.(type) {
		case *proto.Hello:
			kinds = append(kinds, "hello")
		case *proto.Sample:
			kinds = append(kinds, "sample")
			ats = append(ats, x.At.Unix())
		}
	}
	if len(kinds) < 4 || kinds[0] != "hello" {
		t.Fatalf("order = %v", kinds)
	}
	if len(ats) < 3 || ats[0] != 100 || ats[1] != 110 {
		t.Fatalf("the two buffered samples must go out first, got %v", ats)
	}
	if buf.Len() != 0 {
		t.Fatal("buffer must be empty after replay")
	}
}

func TestSessionBuffersSampleWhenSendFails(t *testing.T) {
	fc := &failingConn{reads: make(chan []byte, 1)}
	welcome, _ := proto.Encode(&proto.Welcome{Type: proto.TypeWelcome, IntervalSec: 1})
	fc.reads <- welcome
	buf := NewBuffer(60)
	cfg := Config{Dial: func(context.Context, string, string) (Conn, error) { return fc, nil },
		Log: slog.New(slog.NewTextHandler(discard{}, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sampler, _ := counter()
	err := session(ctx, cfg, sampler, buf)
	if !errors.Is(err, errWelcomed) {
		t.Fatalf("err = %v", err)
	}
	if buf.Len() != 1 {
		t.Fatalf("the sample that failed to send must be kept, buffer holds %d", buf.Len())
	}
}

// failingConn accepts the hello, then fails every later write.
type failingConn struct {
	mu    sync.Mutex
	n     int
	reads chan []byte
}

func (f *failingConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case d := <-f.reads:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *failingConn) Write(context.Context, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == 1 {
		return nil // the hello goes through
	}
	return errors.New("broken pipe")
}

func (f *failingConn) Close() error { return nil }

func TestRunStopsOnContext(t *testing.T) {
	h := newFakeHub(t, 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	sampler, _ := counter()
	go func() { done <- Run(ctx, cfgFor(h), sampler) }()
	waitFor(t, func() bool { _, hellos, _ := h.count(); return hellos == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunKeepsRetryingWhenHubIsDown(t *testing.T) {
	cfg := Config{HubURL: "ws://127.0.0.1:1", Token: "tok", Insecure: true, Hello: proto.Hello{Type: proto.TypeHello},
		Backoff: func(int) time.Duration { return 10 * time.Millisecond }, Log: slog.New(slog.NewTextHandler(discard{}, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sampler, n := counter()
	Run(ctx, cfg, sampler)
	if n() != 0 {
		t.Fatalf("must not sample while disconnected, sampled %d times", n())
	}
}

func TestRunRejectsBadToken(t *testing.T) {
	h := newFakeHub(t, 1, 0)
	cfg := cfgFor(h)
	cfg.Token = "wrong"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sampler, n := counter()
	Run(ctx, cfg, sampler)
	if _, _, samples := h.count(); samples != 0 || n() != 0 {
		t.Fatal("a rejected token must never produce a sample")
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{"wss://hub.example", "ws://localhost:8090", "ws://127.0.0.1:8090", "ws://[::1]:8090"}
	for _, u := range ok {
		if err := ValidateURL(u, false); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if err := ValidateURL("ws://hub.example", false); err == nil {
		t.Fatal("plain ws to a remote host must be refused without --insecure")
	}
	if err := ValidateURL("ws://hub.example", true); err != nil {
		t.Fatal("insecure must allow it")
	}
	if err := ValidateURL("http://hub.example", true); err == nil {
		t.Fatal("scheme must be ws or wss")
	}
}

func TestDefaultBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for i := 0; i < 5; i++ {
		d := defaultBackoff(i)
		if d <= prev {
			t.Fatalf("backoff must grow: attempt %d gave %v after %v", i, d, prev)
		}
		prev = d
	}
	for i := 6; i < 20; i++ {
		if d := defaultBackoff(i); d > 70*time.Second {
			t.Fatalf("backoff must stay capped, attempt %d gave %v", i, d)
		}
	}
}
