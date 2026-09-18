package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Recorder is the part of the store the dispatcher touches. The delivery row
// already exists as pending when a job arrives; the worker only settles it.
type Recorder interface {
	MarkDeliverySent(id int64, now time.Time, attempts int) error
	MarkDeliveryFailed(id int64, now time.Time, attempts int, reason string) error
}

// Job is one message owed to one channel, against one recorded delivery.
type Job struct {
	DeliveryID int64
	Channel    Channel
	Message    Message
}

type Options struct {
	Queue    int
	Attempts int
	Base     time.Duration
	Factor   int
	// Sleep waits d and reports whether the wait completed. Tests replace it to
	// make the schedule instantaneous and observable.
	Sleep func(ctx context.Context, d time.Duration) bool
	Now   func() time.Time
	Log   *slog.Logger
}

// Dispatcher owns the queue and the single worker that drains it.
//
// The queue is a slice behind a mutex rather than a buffered channel, because
// the drop policy has to inspect what is already queued and a channel cannot
// be inspected.
type Dispatcher struct {
	rec  Recorder
	opt  Options
	mu   sync.Mutex
	jobs []Job
	wake chan struct{}
	done chan struct{}
}

func NewDispatcher(rec Recorder, opt Options) *Dispatcher {
	if opt.Queue <= 0 {
		opt.Queue = 256
	}
	if opt.Attempts <= 0 {
		opt.Attempts = 4
	}
	if opt.Base <= 0 {
		opt.Base = time.Second
	}
	if opt.Factor <= 0 {
		opt.Factor = 5
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	if opt.Sleep == nil {
		opt.Sleep = sleepCtx
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Dispatcher{rec: rec, opt: opt, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// sleepCtx waits d, or returns false as soon as ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Backoff is the wait before attempt+1: 1 s, 5 s, 25 s.
func (d *Dispatcher) Backoff(attempt int) time.Duration {
	w := d.opt.Base
	for i := 1; i < attempt; i++ {
		w *= time.Duration(d.opt.Factor)
	}
	return w
}

// Enqueue never blocks. When the queue is full it drops the oldest job it can
// afford to lose — a fired before a resolved — and records that drop as a
// failed delivery, so nothing ever claims a delivery that never left.
func (d *Dispatcher) Enqueue(jobs ...Job) {
	d.mu.Lock()
	var dropped []Job
	for _, j := range jobs {
		if len(d.jobs) >= d.opt.Queue {
			idx := d.victim()
			dropped = append(dropped, d.jobs[idx])
			d.jobs = append(d.jobs[:idx], d.jobs[idx+1:]...)
		}
		d.jobs = append(d.jobs, j)
	}
	d.mu.Unlock()
	for _, j := range dropped {
		d.opt.Log.Warn("notification queue full, delivery dropped",
			"channel", j.Channel.Name(), "host", j.Message.HostName, "kind", j.Message.Kind)
		if err := d.rec.MarkDeliveryFailed(j.DeliveryID, d.opt.Now(), 0, "queue full"); err != nil {
			d.opt.Log.Error("record dropped delivery", "err", err)
		}
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// victim picks the oldest fired job, or the oldest job of any kind when every
// queued job is a recovery. Caller holds the lock.
func (d *Dispatcher) victim() int {
	for i, j := range d.jobs {
		if j.Message.Fired() {
			return i
		}
	}
	return 0
}

// queued returns the delivery ids currently waiting, oldest first. Tests only.
func (d *Dispatcher) queued() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]int64, len(d.jobs))
	for i, j := range d.jobs {
		out[i] = j.DeliveryID
	}
	return out
}

func (d *Dispatcher) pop() (Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.jobs) == 0 {
		return Job{}, false
	}
	j := d.jobs[0]
	d.jobs = d.jobs[1:]
	return j, true
}

// Start runs the single worker until ctx is cancelled.
func (d *Dispatcher) Start(ctx context.Context) {
	go func() {
		defer close(d.done)
		for {
			j, ok := d.pop()
			if !ok {
				select {
				case <-ctx.Done():
					return
				case <-d.wake:
					continue
				}
			}
			d.deliver(ctx, j)
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

// Wait blocks until the worker has stopped. Tests only.
func (d *Dispatcher) Wait() { <-d.done }

func (d *Dispatcher) deliver(ctx context.Context, j Job) {
	var last error
	// used counts what actually happened, rather than being reconstructed from
	// the last error's class afterwards: a 503 followed by a 400 is two attempts,
	// and deriving the count from the final, non-retryable error would log one.
	used := 0
	for attempt := 1; attempt <= d.opt.Attempts; attempt++ {
		used = attempt
		err := j.Channel.Send(ctx, j.Message)
		if err == nil {
			if err := d.rec.MarkDeliverySent(j.DeliveryID, d.opt.Now(), used); err != nil {
				d.opt.Log.Error("record sent delivery", "err", err)
			}
			return
		}
		last = err
		if !Retryable(err) || attempt == d.opt.Attempts {
			break
		}
		if !d.opt.Sleep(ctx, d.Backoff(attempt)) {
			// Shutting down. The row stays pending and is replayed next boot.
			return
		}
	}
	d.opt.Log.Warn("notification failed", "channel", j.Channel.Name(), "host", j.Message.HostName,
		"kind", j.Message.Kind, "attempts", used, "err", last)
	if err := d.rec.MarkDeliveryFailed(j.DeliveryID, d.opt.Now(), used, truncate(last.Error(), 200)); err != nil {
		d.opt.Log.Error("record failed delivery", "err", err)
	}
}
