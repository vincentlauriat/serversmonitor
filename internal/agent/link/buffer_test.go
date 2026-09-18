package link

import (
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

func TestBufferKeepsLastN(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.Push(proto.Sample{At: time.Unix(int64(i), 0)})
	}
	got := b.Drain()
	if len(got) != 3 || got[0].At.Unix() != 2 || got[2].At.Unix() != 4 {
		t.Fatalf("drain = %+v", got)
	}
	if len(b.Drain()) != 0 {
		t.Fatal("drain must empty the buffer")
	}
}
