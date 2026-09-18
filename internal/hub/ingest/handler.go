// Package ingest accepts agent connections and turns their messages into rows.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

// Notifier receives one event per accepted sample so browsers can refresh.
type Notifier interface {
	Publish(typ string, data any)
}

type Config struct {
	IntervalSec     int
	MaxMessageBytes int64
	MinSampleGap    time.Duration
}

func DefaultConfig() Config {
	return Config{IntervalSec: 10, MaxMessageBytes: 256 << 10, MinSampleGap: time.Second}
}

type Handler struct {
	st     *store.Store
	notify Notifier
	log    *slog.Logger

	mu    sync.Mutex
	cfg   Config
	conns map[int64]*websocket.Conn
}

func New(st *store.Store, n Notifier, cfg Config, log *slog.Logger) *Handler {
	return &Handler{st: st, notify: n, cfg: cfg, log: log, conns: map[int64]*websocket.Conn{}}
}

func (h *Handler) config() Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// Connected lists the hosts holding a live socket right now.
func (h *Handler) Connected() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.conns))
	for id := range h.conns {
		out = append(out, id)
	}
	return out
}

// Reconfigure changes the send interval and pushes it to every connected agent
// without making them reconnect.
func (h *Handler) Reconfigure(intervalSec int) {
	h.mu.Lock()
	h.cfg.IntervalSec = intervalSec
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	data, err := proto.Encode(&proto.Reconfigure{Type: proto.TypeReconfigure, IntervalSec: intervalSec})
	if err != nil {
		return
	}
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c.Write(ctx, websocket.MessageText, data)
		cancel()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	host, err := h.st.HostByToken(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(h.config().MaxMessageBytes)
	h.serve(r.Context(), c, host)
}

func (h *Handler) serve(ctx context.Context, c *websocket.Conn, host store.Host) {
	log := h.log.With("host", host.Name, "host_id", host.ID)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The first message must be hello: it carries the machine's identity.
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	msg, err := proto.Decode(data)
	hello, ok := msg.(*proto.Hello)
	if err != nil || !ok {
		c.Close(websocket.StatusProtocolError, "expected hello")
		return
	}
	if err := h.st.UpdateHostInfo(host.ID, store.HostInfo{OS: hello.OS, Arch: hello.Arch, Hostname: hello.Hostname,
		AgentVersion: hello.AgentVersion, Cores: hello.Cores, MemTotal: hello.MemTotal}); err != nil {
		log.Error("update host info", "err", err)
	}
	seen := time.Now().UTC()
	h.st.SetHostStatus(host.ID, "online", &seen)
	welcome, err := proto.Encode(&proto.Welcome{Type: proto.TypeWelcome, IntervalSec: h.config().IntervalSec})
	if err != nil {
		return
	}
	if err := c.Write(ctx, websocket.MessageText, welcome); err != nil {
		return
	}
	h.register(host.ID, c)
	defer h.unregister(host.ID, c)
	h.notify.Publish("host", map[string]any{"id": host.ID})
	log.Info("agent connected", "version", hello.AgentVersion)

	var lastAccepted time.Time
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			switch {
			case errors.As(err, &ce) && ce.Code == websocket.StatusNormalClosure:
				log.Info("agent disconnected")
			case errors.Is(err, context.Canceled):
			default:
				log.Warn("agent read failed", "err", err)
			}
			return
		}
		msg, err := proto.Decode(data)
		if err != nil {
			c.Close(websocket.StatusProtocolError, "malformed message")
			return
		}
		sm, ok := msg.(*proto.Sample)
		if !ok {
			continue // a message type we do not act on
		}
		if gap := h.config().MinSampleGap; gap > 0 && !lastAccepted.IsZero() && time.Since(lastAccepted) < gap {
			continue // rate cap: drop silently
		}
		switch err := h.st.InsertSample(host.ID, sm); {
		case errors.Is(err, store.ErrStale):
			continue
		case err != nil:
			log.Error("insert sample", "err", err)
			continue
		}
		lastAccepted = time.Now()
		seen := time.Now().UTC()
		h.st.SetHostStatus(host.ID, "online", &seen)
		h.notify.Publish("host", map[string]any{"id": host.ID})
	}
}

// register stores the new socket and closes any previous one for that host:
// a restarted agent must not leave a zombie connection behind.
func (h *Handler) register(id int64, c *websocket.Conn) {
	h.mu.Lock()
	old := h.conns[id]
	h.conns[id] = c
	h.mu.Unlock()
	if old != nil {
		old.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
}

func (h *Handler) unregister(id int64, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[id] == c {
		delete(h.conns, id)
	}
}
