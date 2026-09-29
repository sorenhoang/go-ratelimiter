package leakybucket

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// ErrQueueFull is returned when there is no room to wait. A caller gets it
// immediately rather than being parked indefinitely: a queue that accepts
// everyone is not a queue, it is a memory leak with a schedule.
var ErrQueueFull = errors.New("leakybucket: queue is full")

// ErrQueueClosed is returned to callers still waiting when the queue shuts down,
// and to anyone arriving afterwards.
var ErrQueueClosed = errors.New("leakybucket: queue is closed")

// QueueConfig describes the spout and how many callers may line up at it.
type QueueConfig struct {
	// ReleasePerSecond is how fast the queue drains. It sets the interval
	// between releases, so it also sets how evenly spaced the output is.
	ReleasePerSecond float64
	// Capacity is how many callers may be waiting at once. Beyond it, Wait
	// fails fast with ErrQueueFull.
	Capacity int
}

// Queue is the original sense of a leaky bucket: requests line up and leave at
// a steady rate, rather than being judged and refused.
//
// It deliberately does not implement limiter.Limiter. That interface asks
// "may this proceed?" and answers now; this type answers "not yet" and then
// "now", which is a different contract. Bending Wait into AllowN would hide the
// only thing that makes this variant worth having — that it shapes traffic
// instead of rejecting it.
//
// Unlike every other limiter here it is also not per key. Each queue owns a
// goroutine and a ticker, so one per caller would grow without bound. It
// therefore smooths the aggregate flow through an endpoint rather than any one
// client's share of it, which is a different job from the rest of this repo.
//
// A Queue is safe for concurrent use. It must be closed when finished.
type Queue struct {
	// slots is a counting semaphore: a caller takes a place to wait in and
	// gives it back on the way out, whether it was released, cancelled or
	// refused.
	slots chan struct{}
	// releases carries one permit per tick. It is unbuffered, and the sender
	// drops a permit nobody is waiting for, so idle time earns no credit --
	// which is what keeps the output evenly spaced rather than bursty.
	releases chan struct{}

	done      chan struct{}
	closeOnce sync.Once
	worker    sync.WaitGroup
	ticker    *time.Ticker
}

// NewQueue starts a Queue and its worker. The caller must Close it.
func NewQueue(cfg QueueConfig) (*Queue, error) {
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("leakybucket: invalid queue capacity: %d", cfg.Capacity)
	}
	// NaN fails every comparison, so it would slip past a plain `<= 0`.
	if math.IsNaN(cfg.ReleasePerSecond) || math.IsInf(cfg.ReleasePerSecond, 0) || cfg.ReleasePerSecond <= 0 {
		return nil, fmt.Errorf("leakybucket: invalid release rate: %v", cfg.ReleasePerSecond)
	}

	interval := time.Duration(float64(time.Second) / cfg.ReleasePerSecond)
	// time.NewTicker panics on a non-positive interval, which a rate above a
	// billion per second would produce once the division truncates.
	if interval <= 0 {
		return nil, fmt.Errorf("leakybucket: release rate %v is too fast to schedule", cfg.ReleasePerSecond)
	}

	q := &Queue{
		slots:    make(chan struct{}, cfg.Capacity),
		releases: make(chan struct{}),
		done:     make(chan struct{}),
		ticker:   time.NewTicker(interval),
	}

	q.worker.Add(1)
	go q.run()

	return q, nil
}

func (q *Queue) run() {
	defer q.worker.Done()

	for {
		select {
		case <-q.ticker.C:
			// Hand the permit to whoever is waiting, and drop it if nobody is.
			// Buffering it instead would let an idle stretch accumulate permits
			// and release a burst, which is the opposite of the point.
			select {
			case q.releases <- struct{}{}:
			default:
			}
		case <-q.done:
			return
		}
	}
}

// Wait blocks until the caller's turn, and returns nil when it arrives.
//
// It returns ErrQueueFull straight away when there is no room to wait,
// ctx.Err() if the caller gives up first, and ErrQueueClosed if the queue shuts
// down while it is waiting. A caller that gives up releases its place at once
// rather than holding it until its turn would have come.
func (q *Queue) Wait(ctx context.Context) error {
	// Checked before taking a slot, so a closed queue refuses rather than
	// parking the caller until the select below notices.
	select {
	case <-q.done:
		return ErrQueueClosed
	default:
	}

	select {
	case q.slots <- struct{}{}:
	default:
		return ErrQueueFull
	}
	defer func() { <-q.slots }()

	select {
	case <-q.releases:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return ErrQueueClosed
	}
}

// Len reports how many callers are waiting. It is a snapshot, useful for
// reporting rather than for deciding anything.
func (q *Queue) Len() int {
	return len(q.slots)
}

// Close stops the worker and releases every waiting caller with
// ErrQueueClosed. It is safe to call more than once, and safe to call while
// callers are waiting.
func (q *Queue) Close() error {
	q.closeOnce.Do(func() {
		// Closing done, rather than closing releases, is what makes this safe:
		// a closed releases channel would hand every waiter a zero value that
		// reads exactly like a permit.
		close(q.done)
		q.ticker.Stop()
	})
	q.worker.Wait()
	return nil
}
