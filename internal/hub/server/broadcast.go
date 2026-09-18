// Package server exposes the hub over HTTP: JSON API, SSE and the embedded front.
package server

import "sync"

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Broadcaster fans one event out to every open browser stream.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewBroadcaster() *Broadcaster { return &Broadcaster{subs: map[chan Event]struct{}{}} }

// Publish never blocks: a subscriber that cannot keep up loses events rather
// than stalling the ingest path that calls this.
func (b *Broadcaster) Publish(typ string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- Event{Type: typ, Data: data}:
		default:
		}
	}
}

func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
}
