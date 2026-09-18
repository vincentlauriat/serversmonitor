package server

import (
	"testing"
	"time"
)

func TestBroadcasterDeliversAndUnsubscribes(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.Subscribe()
	b.Publish("host", map[string]any{"id": 1})
	select {
	case ev := <-ch:
		if ev.Type != "host" {
			t.Fatalf("ev = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	cancel()
	b.Publish("host", nil) // must not block or panic
	if _, ok := <-ch; ok {
		t.Fatal("channel must be closed after cancel")
	}
	cancel() // cancelling twice must not panic
}

func TestBroadcasterDropsWhenSubscriberIsSlow(t *testing.T) {
	b := NewBroadcaster()
	_, cancel := b.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish("host", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish must never block on a slow subscriber")
	}
}
