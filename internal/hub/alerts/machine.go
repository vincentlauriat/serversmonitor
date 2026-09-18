// Package alerts turns samples and rules into fired/resolved events.
package alerts

import (
	"sync"
	"time"
)

type State int

const (
	Ok State = iota
	Pending
	Firing
)

// Key identifies one rule applied to one host.
type Key struct{ RuleID, HostID int64 }

type Transition int

const (
	None Transition = iota
	Fired
	Resolved
)

type cell struct {
	state State
	since time.Time // first breach of the current pending run
}

type Machine struct {
	mu    sync.Mutex
	cells map[Key]*cell
}

func NewMachine() *Machine { return &Machine{cells: map[Key]*cell{}} }

// Restore marks keys as already firing, so a hub restart does not re-announce
// alerts that were firing before it went down.
func (m *Machine) Restore(firing []Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range firing {
		m.cells[k] = &cell{state: Firing}
	}
}

func (m *Machine) State(k Key) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cells[k]; ok {
		return c.state
	}
	return Ok
}

func (m *Machine) Forget(k Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cells, k)
}

// Observe feeds one observation and returns the transition it caused, if any.
// Only pending->firing and firing->ok are reported; pending->ok is silent,
// which is what keeps a flapping metric out of the log.
func (m *Machine) Observe(k Key, breached bool, now time.Time, d time.Duration) Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cells[k]
	if !ok {
		c = &cell{}
		m.cells[k] = c
	}
	switch c.state {
	case Ok:
		if !breached {
			return None
		}
		c.state, c.since = Pending, now
		if d <= 0 {
			c.state = Firing
			return Fired
		}
		return None
	case Pending:
		if !breached {
			c.state = Ok
			return None
		}
		if now.Sub(c.since) >= d {
			c.state = Firing
			return Fired
		}
		return None
	case Firing:
		if breached {
			return None
		}
		c.state = Ok
		return Resolved
	}
	return None
}
