package periodic

import (
	"context"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// Batcher coalesces high-frequency items into periodic bulk flushes.
// Producers call Add (best-effort: it never wedges the producer once
// shutdown begins); a single Run goroutine drains the buffer to Flush
// either when it reaches MaxBatch items or every Interval, whichever
// comes first. On ctx cancellation Run drains whatever is still buffered
// with one final Flush before returning, so a graceful shutdown does not
// lose the in-memory batch.
//
// It is the write-coalescing companion to Run (this file's sibling): a
// stream that produces thousands of items/second but whose sink (Mongo,
// an HTTP API) wants a handful of bulk writes/minute feeds a Batcher
// instead of writing per item.
//
//	b := periodic.NewBatcher(periodic.BatcherConfig[Domain]{
//	    Name:     "domain_upsert",
//	    Interval: 10 * time.Second,
//	    MaxBatch: 500,
//	    Flush:    store.UpsertDomains,
//	})
//	go b.Run(ctx)        // one draining goroutine
//	b.Add(ctx, domain)   // from the producer(s)
type Batcher[T any] struct {
	name         string
	interval     time.Duration
	maxBatch     int
	flushTimeout time.Duration
	flush        func(ctx context.Context, items []T) error
	logger       logger
	ch           chan T
}

// BatcherConfig configures a Batcher. Name, Interval, MaxBatch and Flush
// are the meaningful knobs; the rest have sane defaults.
type BatcherConfig[T any] struct {
	// Name labels the batcher in flush log records (`name=…`).
	Name string

	// Interval is the maximum time an item waits before its batch is
	// flushed. A flush also fires early when MaxBatch is reached.
	Interval time.Duration

	// MaxBatch flushes the buffer early once it holds this many items,
	// bounding both flush size and the memory the buffer can hold.
	MaxBatch int

	// BufferSize is the channel capacity between producers and the Run
	// goroutine. Zero defaults to MaxBatch*4 so a brief producer burst
	// does not block while a flush is in flight.
	BufferSize int

	// FlushTimeout caps a single Flush call. Zero defaults to 30s. Each
	// flush runs under a fresh background-derived context so the final
	// shutdown drain still completes after the caller's ctx is cancelled.
	FlushTimeout time.Duration

	// Flush writes one batch to the sink. A non-nil error is logged and
	// the batch is dropped (the items are gone); make Flush idempotent
	// and tolerant of partial failure (e.g. duplicate-safe upserts).
	Flush func(ctx context.Context, items []T) error

	// Logger records flush OK / error events. nil = log.Default.
	Logger logger
}

const (
	defaultBatchInterval     = 10 * time.Second
	defaultMaxBatch          = 500
	defaultBatchFlushTimeout = 30 * time.Second
)

// NewBatcher builds a Batcher from cfg, applying defaults for the unset
// fields. It does not start anything: call Run in a goroutine.
func NewBatcher[T any](cfg BatcherConfig[T]) *Batcher[T] {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultBatchInterval
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultMaxBatch
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = defaultBatchFlushTimeout
	}
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = cfg.MaxBatch * 4
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.Default()
	}
	return &Batcher[T]{
		name:         cfg.Name,
		interval:     cfg.Interval,
		maxBatch:     cfg.MaxBatch,
		flushTimeout: cfg.FlushTimeout,
		flush:        cfg.Flush,
		logger:       lg,
		ch:           make(chan T, bufSize),
	}
}

// Add hands an item to the batcher. It blocks only until the buffered
// channel has room or ctx is cancelled (then the item is dropped), so it
// never wedges the producer after shutdown begins. Safe for concurrent
// producers.
func (b *Batcher[T]) Add(ctx context.Context, item T) {
	select {
	case b.ch <- item:
	case <-ctx.Done():
	}
}

// Run is the single draining goroutine. It returns when ctx is
// cancelled, after a final flush of whatever is still buffered.
func (b *Batcher[T]) Run(ctx context.Context) {
	buf := make([]T, 0, b.maxBatch)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.drain(&buf)
			return
		case item := <-b.ch:
			buf = append(buf, item)
			if len(buf) >= b.maxBatch {
				b.flushBuf(&buf)
			}
		case <-ticker.C:
			b.flushBuf(&buf)
		}
	}
}

// drain empties whatever is still in the channel (non-blocking) into buf
// and flushes, so the in-flight batch survives a shutdown. It flushes
// mid-drain when the buffer fills so a backlog does not grow unbounded.
func (b *Batcher[T]) drain(buf *[]T) {
	for {
		select {
		case item := <-b.ch:
			*buf = append(*buf, item)
			if len(*buf) >= b.maxBatch {
				b.flushBuf(buf)
			}
		default:
			b.flushBuf(buf)
			return
		}
	}
}

// flushBuf flushes buf if non-empty and resets it. The flush runs under
// a fresh background-derived context (not the caller's, which may be the
// one being cancelled at shutdown) so the final drain completes.
func (b *Batcher[T]) flushBuf(buf *[]T) {
	if len(*buf) == 0 {
		return
	}
	items := make([]T, len(*buf))
	copy(items, *buf)
	*buf = (*buf)[:0]

	ctx, cancel := context.WithTimeout(context.Background(), b.flushTimeout)
	defer cancel()

	start := time.Now()
	err := b.flush(ctx, items)
	durMS := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		b.logger.Error("batch flush failed",
			"name", b.name, "items", len(items), "duration_ms", durMS, "error", err)
		return
	}
	b.logger.Info("batch flushed",
		"name", b.name, "items", len(items), "duration_ms", durMS)
}
