package link

import "github.com/vincentlauriat/serversmonitor/internal/proto"

// Buffer keeps the last n samples that could not be sent, oldest first, so a
// short outage leaves a gap in the charts no longer than the outage itself.
type Buffer struct {
	n     int
	items []proto.Sample
}

func NewBuffer(n int) *Buffer { return &Buffer{n: n} }

func (b *Buffer) Push(s proto.Sample) {
	b.items = append(b.items, s)
	if len(b.items) > b.n {
		b.items = b.items[len(b.items)-b.n:]
	}
}

func (b *Buffer) Drain() []proto.Sample {
	out := b.items
	b.items = nil
	return out
}

func (b *Buffer) Len() int { return len(b.items) }
