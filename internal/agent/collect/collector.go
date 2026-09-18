// Package collect gathers system and Docker metrics behind small interfaces
// so the agent can be tested without touching /proc or a Docker socket.
package collect

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

// ErrUnavailable means the source itself is absent, not that it reported zero.
var ErrUnavailable = errors.New("collect: unavailable")

type DiskUsage struct {
	Mount                 string
	Used, Total           int64
	ReadBytes, WriteBytes int64
}

type ContainerStat struct {
	Name, Image, Status string
	CPU                 float64
	MemUsed             int64
	NetSent, NetRecv    int64
}

type SystemReader interface {
	CPUPercent(ctx context.Context) (float64, error)
	Memory(ctx context.Context) (used, total, swapUsed, swapTotal int64, err error)
	Load(ctx context.Context) (l1, l5, l15 float64, err error)
	Uptime(ctx context.Context) (int64, error)
	Disks(ctx context.Context) ([]DiskUsage, error)
	Net(ctx context.Context) (sentTotal, recvTotal int64, err error)
	Temps(ctx context.Context) ([]proto.Temp, error)
}

type DockerReader interface {
	Containers(ctx context.Context) ([]ContainerStat, error)
}

// counters holds the previous cumulative readings, so rates can be derived.
type counters struct {
	at        time.Time
	diskRead  map[string]int64
	diskWrite map[string]int64
	netSent   int64
	netRecv   int64
	ctrSent   map[string]int64
	ctrRecv   map[string]int64
	haveNet   bool
}

type Collector struct {
	sys    SystemReader
	docker DockerReader
	log    *slog.Logger

	mu           sync.Mutex
	ignoreMounts []string
	prev         *counters
	warned       map[string]bool
}

func New(sys SystemReader, docker DockerReader, ignoreMounts []string, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{sys: sys, docker: docker, ignoreMounts: ignoreMounts, log: log, warned: map[string]bool{}}
}

func (c *Collector) SetIgnoreMounts(m []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ignoreMounts = m
}

// warnOnce logs a reader failure the first time only: a Pi without sensors
// would otherwise log the same line every ten seconds forever.
func (c *Collector) warnOnce(section string, err error) {
	if c.warned[section] {
		return
	}
	c.warned[section] = true
	c.log.Warn("section unavailable", "section", section, "err", err)
}

// rate turns two cumulative readings into a per-second rate. A counter that
// went backwards means the machine or the interface restarted: report 0, not a
// negative spike.
func rate(cur, prev int64, dt time.Duration) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / dt.Seconds()
}

func (c *Collector) ignored(mount string) bool {
	for _, p := range c.ignoreMounts {
		if mount == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(mount, p)) {
			return true
		}
	}
	return false
}

// Collect builds one sample. Every section is independent: a reader that fails
// leaves its fields nil (not collected) and never prevents the others.
func (c *Collector) Collect(ctx context.Context, now time.Time) proto.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := proto.Sample{Type: proto.TypeSample, At: now.UTC()}
	next := &counters{at: now, diskRead: map[string]int64{}, diskWrite: map[string]int64{}, ctrSent: map[string]int64{}, ctrRecv: map[string]int64{}}
	var dt time.Duration
	if c.prev != nil {
		dt = now.Sub(c.prev.at)
	}

	if v, err := c.sys.CPUPercent(ctx); err != nil {
		c.warnOnce("cpu", err)
	} else {
		s.CPU = &v
	}
	if used, total, swapUsed, swapTotal, err := c.sys.Memory(ctx); err != nil {
		c.warnOnce("memory", err)
	} else {
		s.MemUsed, s.MemTotal, s.SwapUsed, s.SwapTotal = &used, &total, &swapUsed, &swapTotal
	}
	if l1, l5, l15, err := c.sys.Load(ctx); err != nil {
		c.warnOnce("load", err)
	} else {
		s.Load1, s.Load5, s.Load15 = &l1, &l5, &l15
	}
	if up, err := c.sys.Uptime(ctx); err != nil {
		c.warnOnce("uptime", err)
	} else {
		s.Uptime = &up
	}
	if disks, err := c.sys.Disks(ctx); err != nil {
		c.warnOnce("disks", err)
	} else {
		s.Disks = []proto.Disk{}
		for _, d := range disks {
			if c.ignored(d.Mount) {
				continue
			}
			next.diskRead[d.Mount], next.diskWrite[d.Mount] = d.ReadBytes, d.WriteBytes
			pd := proto.Disk{Mount: d.Mount, Used: d.Used, Total: d.Total}
			if c.prev != nil {
				if pr, ok := c.prev.diskRead[d.Mount]; ok {
					pd.ReadBps = rate(d.ReadBytes, pr, dt)
					pd.WriteBps = rate(d.WriteBytes, c.prev.diskWrite[d.Mount], dt)
				}
			}
			s.Disks = append(s.Disks, pd)
		}
	}
	if sent, recv, err := c.sys.Net(ctx); err != nil {
		c.warnOnce("net", err)
	} else {
		next.netSent, next.netRecv, next.haveNet = sent, recv, true
		n := &proto.Net{SentTotal: sent, RecvTotal: recv}
		if c.prev != nil && c.prev.haveNet {
			n.SentBps = rate(sent, c.prev.netSent, dt)
			n.RecvBps = rate(recv, c.prev.netRecv, dt)
		}
		s.Net = n
	}
	if temps, err := c.sys.Temps(ctx); err != nil {
		c.warnOnce("temps", err)
	} else {
		s.Temps = temps
		if s.Temps == nil {
			s.Temps = []proto.Temp{}
		}
	}
	if c.docker != nil {
		if stats, err := c.docker.Containers(ctx); err != nil {
			c.warnOnce("docker", err)
		} else {
			s.DockerAvailable = true
			s.Containers = make([]proto.Container, 0, len(stats))
			for _, st := range stats {
				next.ctrSent[st.Name], next.ctrRecv[st.Name] = st.NetSent, st.NetRecv
				pc := proto.Container{Name: st.Name, Image: st.Image, Status: st.Status, CPU: st.CPU, MemUsed: st.MemUsed}
				if c.prev != nil {
					if ps, ok := c.prev.ctrSent[st.Name]; ok {
						pc.NetSentBps = rate(st.NetSent, ps, dt)
						pc.NetRecvBps = rate(st.NetRecv, c.prev.ctrRecv[st.Name], dt)
					}
				}
				s.Containers = append(s.Containers, pc)
			}
		}
	}
	c.prev = next
	return s
}
