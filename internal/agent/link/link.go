// Package link keeps one outbound WebSocket to the hub alive and pushes samples through it.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close() error
}

type DialFunc func(ctx context.Context, url, token string) (Conn, error)

type Config struct {
	HubURL    string
	Token     string
	Insecure  bool
	Hello     proto.Hello
	Dial      DialFunc
	Backoff   func(attempt int) time.Duration
	OnWelcome func(w *proto.Welcome)
	Log       *slog.Logger
}

// Sampler is collect.Collector.Collect.
type Sampler func(ctx context.Context, now time.Time) proto.Sample

const bufferSize = 60

// ValidateURL refuses a plain ws:// to anything but the local machine: the
// token travels in a header, and a header on plain HTTP is readable in transit.
func ValidateURL(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		if insecure || host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("refusing plain ws:// to %s: use wss:// or pass --insecure", host)
	}
	return fmt.Errorf("hub url must start with ws:// or wss://")
}

func defaultBackoff(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	d := time.Second << attempt // 1s .. 64s
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 5)) // up to 20 %
	return d - jitter/2 + jitter
}

type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}

func (w wsConn) Write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}

func (w wsConn) Close() error { return w.c.Close(websocket.StatusNormalClosure, "") }

func WebSocketDial(ctx context.Context, u, token string) (Conn, error) {
	c, _, err := websocket.Dial(ctx, strings.TrimSuffix(u, "/")+"/agent/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		return nil, err
	}
	return wsConn{c}, nil
}

// errWelcomed marks an error that happened after a successful handshake, so
// the backoff can start over rather than keep growing.
var errWelcomed = errors.New("after welcome")

// Run connects, reconnects with backoff, and streams samples until ctx is done.
func Run(ctx context.Context, cfg Config, sample Sampler) error {
	if cfg.Dial == nil {
		cfg.Dial = WebSocketDial
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	buf := NewBuffer(bufferSize)
	attempt := 0
	for {
		err := session(ctx, cfg, sample, buf)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errWelcomed) {
			attempt = 0 // we had a real session; do not inherit its predecessor's backoff
		}
		wait := cfg.Backoff(attempt)
		cfg.Log.Warn("hub link lost, retrying", "err", err, "in", wait)
		attempt++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func session(ctx context.Context, cfg Config, sample Sampler, buf *Buffer) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := cfg.Dial(dialCtx, cfg.HubURL, cfg.Token)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	hello := cfg.Hello
	hello.Type = proto.TypeHello
	data, err := proto.Encode(&hello)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, data); err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	data, err = conn.Read(wctx)
	wcancel()
	if err != nil {
		return fmt.Errorf("waiting for welcome: %w", err)
	}
	msg, err := proto.Decode(data)
	if err != nil {
		return err
	}
	welcome, ok := msg.(*proto.Welcome)
	if !ok {
		return fmt.Errorf("expected welcome, got %T", msg)
	}
	if cfg.OnWelcome != nil {
		cfg.OnWelcome(welcome)
	}
	interval := time.Duration(max(welcome.IntervalSec, 1)) * time.Second
	cfg.Log.Info("connected to hub", "interval", interval)

	// Replay what was buffered while we were away, oldest first, before any
	// fresh sample: the hub refuses anything older than its newest row.
	for _, s := range buf.Drain() {
		if err := send(ctx, conn, &s); err != nil {
			buf.Push(s)
			return fmt.Errorf("%w: replay: %v", errWelcomed, err)
		}
	}

	incoming := make(chan any, 4)
	readErr := make(chan error, 1)
	go func() {
		for {
			data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if m, err := proto.Decode(data); err == nil {
				select {
				case incoming <- m:
				default:
				}
			}
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return fmt.Errorf("%w: read: %v", errWelcomed, err)
		case m := <-incoming:
			if rc, ok := m.(*proto.Reconfigure); ok && rc.IntervalSec > 0 {
				interval = time.Duration(rc.IntervalSec) * time.Second
				ticker.Reset(interval)
				cfg.Log.Info("interval reconfigured", "interval", interval)
			}
		case now := <-ticker.C:
			s := sample(ctx, now)
			if err := send(ctx, conn, &s); err != nil {
				buf.Push(s)
				return fmt.Errorf("%w: send: %v", errWelcomed, err)
			}
		}
	}
}

func send(ctx context.Context, conn Conn, s *proto.Sample) error {
	s.Type = proto.TypeSample
	data, err := proto.Encode(s)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, data)
}
