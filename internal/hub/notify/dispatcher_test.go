package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder records what the dispatcher decided, which is the whole contract.
type recorder struct {
	mu       sync.Mutex
	sent     map[int64]int
	failed   map[int64]string
	attempts map[int64]int // attempts recorded on failure
	done     chan struct{}
	want     int
	seen     int
}

func newRecorder(want int) *recorder {
	return &recorder{sent: map[int64]int{}, failed: map[int64]string{}, attempts: map[int64]int{},
		done: make(chan struct{}), want: want}
}

// settle is called with the lock held.
func (r *recorder) settle() {
	r.seen++
	if r.seen == r.want {
		close(r.done)
	}
}

func (r *recorder) MarkDeliverySent(id int64, _ time.Time, attempts int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent[id] = attempts
	r.settle()
	return nil
}

func (r *recorder) MarkDeliveryFailed(id int64, _ time.Time, attempts int, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed[id] = reason
	r.attempts[id] = attempts
	r.settle()
	return nil
}

func (r *recorder) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.mu.Lock()
		defer r.mu.Unlock()
		t.Fatalf("timed out: %d of %d settled (sent=%v failed=%v)", r.seen, r.want, r.sent, r.failed)
	}
}

func (r *recorder) sentAttempts(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[id]
}

func (r *recorder) failReason(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed[id]
}

// fakeChannel answers from a script: one entry per attempt, the last reused.
type fakeChannel struct {
	name   string
	script []error
	calls  atomic.Int32
	seen   chan Message
}

func newFake(name string, script ...error) *fakeChannel {
	return &fakeChannel{name: name, script: script, seen: make(chan Message, 32)}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Send(ctx context.Context, m Message) error {
	n := int(f.calls.Add(1)) - 1
	f.seen <- m
	if len(f.script) == 0 {
		return nil
	}
	if n < len(f.script) {
		return f.script[n]
	}
	return f.script[len(f.script)-1]
}

// testOptions makes the retry schedule instantaneous while still observable.
func testOptions(slept *[]time.Duration, mu *sync.Mutex) Options {
	return Options{Sleep: func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		*slept = append(*slept, d)
		mu.Unlock()
		return true
	}}
}

func TestDispatcherSendsOnceOnSuccess(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("smtp")
	d.Enqueue(Job{DeliveryID: 7, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("channel called %d times", got)
	}
	if rec.sentAttempts(7) != 1 {
		t.Fatalf("sent attempts = %d", rec.sentAttempts(7))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 0 {
		t.Fatalf("a success must not sleep: %v", slept)
	}
	if m := <-ch.seen; m.HostName != "mac-vincent" {
		t.Fatalf("message = %+v", m)
	}
}

func TestDispatcherRetriesRetryableFailures(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("webhook", MarkRetryable(errors.New("503")), MarkRetryable(errors.New("503")), nil)
	d.Enqueue(Job{DeliveryID: 9, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 3 {
		t.Fatalf("channel called %d times, want 3", got)
	}
	if rec.sentAttempts(9) != 3 {
		t.Fatalf("the recorded attempt count must be the real one: %d", rec.sentAttempts(9))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 5*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s]", slept)
	}
}

func TestDispatcherGivesUpAfterFourAttempts(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("teams", MarkRetryable(errors.New("boom")))
	d.Enqueue(Job{DeliveryID: 11, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 4 {
		t.Fatalf("channel called %d times, want 4", got)
	}
	if reason := rec.failReason(11); reason != "boom" {
		t.Fatalf("failure reason = %q", reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 3 || slept[2] != 25*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s 25s]", slept)
	}
}

func TestDispatcherDoesNotRetryPermanentFailures(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("webhook", errors.New("Bad Request: metric unknown"))
	d.Enqueue(Job{DeliveryID: 13, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("a 400 must be tried once, was tried %d times", got)
	}
	if rec.failReason(13) != "Bad Request: metric unknown" {
		t.Fatalf("failed = %q", rec.failReason(13))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 0 {
		t.Fatalf("no sleeping between attempts that never happen: %v", slept)
	}
}

type blockingChannel struct{ release chan struct{} }

func (b *blockingChannel) Name() string { return "slow" }

func (b *blockingChannel) Send(ctx context.Context, m Message) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func TestEnqueueNeverBlocks(t *testing.T) {
	// The evaluator calls Enqueue on the hub's own goroutine. If it ever blocked,
	// a dead webhook would stop the hub from evaluating alerts at all.
	rec := newRecorder(0)
	block := make(chan struct{})
	slow := &blockingChannel{release: block}
	d := NewDispatcher(rec, Options{Queue: 1, Sleep: func(context.Context, time.Duration) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	done := make(chan struct{})
	go func() {
		for i := int64(0); i < 200; i++ {
			d.Enqueue(Job{DeliveryID: i, Channel: slow, Message: fired()})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Enqueue blocked")
	}
	close(block)
}

func TestQueueFullDropsTheOldestFiredAndKeepsResolved(t *testing.T) {
	// A dropped alert is bad. A dropped recovery is worse: the last thing
	// Vincent ever heard about that host is that it broke.
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Queue: 2, Sleep: func(context.Context, time.Duration) bool { return true }})
	ch := newFake("smtp")
	resolved := fired()
	resolved.Kind = "resolved"
	// No worker started: the queue fills and stays full.
	d.Enqueue(Job{DeliveryID: 1, Channel: ch, Message: fired()})
	d.Enqueue(Job{DeliveryID: 2, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 3, Channel: ch, Message: fired()})
	ids := d.queued()
	if len(ids) != 2 {
		t.Fatalf("queue = %v, want 2 entries", ids)
	}
	if ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("queue = %v, want the resolved (2) kept and the oldest fired (1) dropped", ids)
	}
	if reason := rec.failReason(1); reason != "queue full" {
		t.Fatalf("a dropped delivery must be recorded as failed, got %q", reason)
	}
}

func TestQueueFullDropsAResolvedOnlyAsALastResort(t *testing.T) {
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Queue: 2, Sleep: func(context.Context, time.Duration) bool { return true }})
	ch := newFake("smtp")
	resolved := fired()
	resolved.Kind = "resolved"
	d.Enqueue(Job{DeliveryID: 1, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 2, Channel: ch, Message: resolved})
	d.Enqueue(Job{DeliveryID: 3, Channel: ch, Message: resolved})
	ids := d.queued()
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("queue = %v, want the oldest resolved dropped when nothing else can be", ids)
	}
	if rec.failReason(1) != "queue full" {
		t.Fatalf("failed = %q", rec.failReason(1))
	}
}

func TestShutdownStopsRetryingAndLeavesTheRowPending(t *testing.T) {
	// Deterministic by construction rather than by timing: the real sleepCtx is
	// used with an hour-long base, so after the first failure the worker is
	// parked inside the backoff and can only leave it by cancellation.
	rec := newRecorder(0)
	d := NewDispatcher(rec, Options{Base: time.Hour}) // Sleep nil = the real one
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	ch := newFake("teams", MarkRetryable(errors.New("down")))
	d.Enqueue(Job{DeliveryID: 5, Channel: ch, Message: fired()})
	<-ch.seen // the first attempt happened and failed
	cancel()
	d.Wait()
	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("a cancelled dispatcher kept retrying: %d attempts", got)
	}
	// Nothing settled: the row stays pending, which is what makes the replay on
	// the next boot the right thing to do rather than a duplicate.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sent) != 0 || len(rec.failed) != 0 {
		t.Fatalf("a shutdown must settle nothing, sent=%v failed=%v", rec.sent, rec.failed)
	}
}

func TestBackoffSchedule(t *testing.T) {
	d := NewDispatcher(newRecorder(0), Options{})
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 5 * time.Second, 3: 25 * time.Second} {
		if got := d.Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestMixedFailuresRecordTheAttemptsThatHappened(t *testing.T) {
	// A 503 then a 400: two attempts happened, and the log must say two. Deriving
	// the count from the last error's class reports one, because the last error
	// is the one that is not retryable. The delivery log is the artifact this
	// whole lot exists to produce; a number in it that is sometimes false is
	// worse than no number.
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("webhook", MarkRetryable(errors.New("503")), errors.New("400 Bad Request"))
	d.Enqueue(Job{DeliveryID: 21, Channel: ch, Message: fired()})
	rec.wait(t)
	if got := ch.calls.Load(); got != 2 {
		t.Fatalf("channel called %d times, want 2", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.attempts[21] != 2 {
		t.Fatalf("recorded attempts = %d, want the 2 that actually happened", rec.attempts[21])
	}
	if rec.failed[21] != "400 Bad Request" {
		t.Fatalf("reason = %q, want the last error", rec.failed[21])
	}
}

func TestPermanentFailureRecordsOneAttempt(t *testing.T) {
	rec := newRecorder(1)
	var mu sync.Mutex
	var slept []time.Duration
	d := NewDispatcher(rec, testOptions(&slept, &mu))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	ch := newFake("smtp", errors.New("550 no such user"))
	d.Enqueue(Job{DeliveryID: 23, Channel: ch, Message: fired()})
	rec.wait(t)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.attempts[23] != 1 {
		t.Fatalf("recorded attempts = %d, want 1", rec.attempts[23])
	}
}
