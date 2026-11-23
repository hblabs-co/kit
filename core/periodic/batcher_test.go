package periodic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// noopLogger silences flush logs in tests that don't assert on them.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// recorder is a test Flush sink: it records each batch and, when got is
// non-nil, signals every flush so a test can await it without sleeping.
type recorder struct {
	mu      sync.Mutex
	batches [][]int
	got     chan []int
	err     error
}

func (r *recorder) flush(_ context.Context, items []int) error {
	cp := append([]int(nil), items...)
	r.mu.Lock()
	r.batches = append(r.batches, cp)
	r.mu.Unlock()
	if r.got != nil {
		r.got <- cp
	}
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, b := range r.batches {
		n += len(b)
	}
	return n
}

// await reads one flushed batch or fails after a generous deadline.
func await(t *testing.T, got chan []int) []int {
	t.Helper()
	select {
	case b := <-got:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a flush")
		return nil
	}
}

// TestNewBatcher_Defaults exercises every default branch: zero Interval,
// MaxBatch, FlushTimeout, BufferSize and a nil Logger all fall back.
func TestNewBatcher_Defaults(t *testing.T) {
	b := NewBatcher(BatcherConfig[int]{}) // everything zero
	if b.interval != defaultBatchInterval {
		t.Errorf("interval = %v, want %v", b.interval, defaultBatchInterval)
	}
	if b.maxBatch != defaultMaxBatch {
		t.Errorf("maxBatch = %d, want %d", b.maxBatch, defaultMaxBatch)
	}
	if b.flushTimeout != defaultBatchFlushTimeout {
		t.Errorf("flushTimeout = %v, want %v", b.flushTimeout, defaultBatchFlushTimeout)
	}
	if cap(b.ch) != defaultMaxBatch*4 {
		t.Errorf("buffer cap = %d, want %d", cap(b.ch), defaultMaxBatch*4)
	}
	if b.logger == nil {
		t.Error("logger should default to log.Default(), got nil")
	}
}

// TestNewBatcher_ExplicitValues keeps caller-supplied knobs intact.
func TestNewBatcher_ExplicitValues(t *testing.T) {
	b := NewBatcher(BatcherConfig[int]{
		Name: "x", Interval: time.Second, MaxBatch: 7,
		BufferSize: 9, FlushTimeout: 2 * time.Second, Logger: noopLogger{},
	})
	if b.interval != time.Second || b.maxBatch != 7 || cap(b.ch) != 9 || b.flushTimeout != 2*time.Second {
		t.Errorf("explicit config not honoured: %+v", b)
	}
}

// TestBatcher_FlushOnMaxBatch — reaching MaxBatch flushes early, before
// the (long) interval fires.
func TestBatcher_FlushOnMaxBatch(t *testing.T) {
	rec := &recorder{got: make(chan []int, 4)}
	b := NewBatcher(BatcherConfig[int]{
		Name: "max", Interval: time.Hour, MaxBatch: 3,
		Flush: rec.flush, Logger: noopLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	for i := 0; i < 3; i++ {
		b.Add(ctx, i)
	}
	if batch := await(t, rec.got); len(batch) != 3 {
		t.Errorf("flushed %d items, want 3", len(batch))
	}
}

// TestBatcher_FlushOnInterval — a sub-MaxBatch buffer still flushes when
// the interval elapses.
func TestBatcher_FlushOnInterval(t *testing.T) {
	rec := &recorder{got: make(chan []int, 4)}
	b := NewBatcher(BatcherConfig[int]{
		Name: "tick", Interval: 15 * time.Millisecond, MaxBatch: 100,
		Flush: rec.flush, Logger: noopLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.Add(ctx, 1)
	b.Add(ctx, 2)
	if batch := await(t, rec.got); len(batch) != 2 {
		t.Errorf("interval flush = %d items, want 2", len(batch))
	}
}

// TestBatcher_DrainsOnShutdown — cancelling ctx flushes whatever is
// buffered exactly once, so the in-memory batch is not lost.
func TestBatcher_DrainsOnShutdown(t *testing.T) {
	rec := &recorder{got: make(chan []int, 4)}
	b := NewBatcher(BatcherConfig[int]{
		Name: "drain", Interval: time.Hour, MaxBatch: 100,
		Flush: rec.flush, Logger: noopLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	b.Add(ctx, 10)
	b.Add(ctx, 20)
	cancel() // triggers the drain path

	if batch := await(t, rec.got); len(batch) != 2 {
		t.Errorf("shutdown drain = %d items, want 2", len(batch))
	}
}

// TestBatcher_DrainBacklogFlushesByMaxBatch white-boxes drain(): a
// channel backlog larger than MaxBatch must flush in MaxBatch-sized
// chunks plus a final partial, never one oversized flush.
func TestBatcher_DrainBacklogFlushesByMaxBatch(t *testing.T) {
	rec := &recorder{} // synchronous: inspect batches after
	b := NewBatcher(BatcherConfig[int]{
		Name: "backlog", Interval: time.Hour, MaxBatch: 2,
		BufferSize: 10, Flush: rec.flush, Logger: noopLogger{},
	})
	for i := 0; i < 5; i++ {
		b.ch <- i
	}
	var buf []int
	b.drain(&buf)

	rec.mu.Lock()
	batches := rec.batches
	rec.mu.Unlock()

	total := 0
	for _, bt := range batches {
		total += len(bt)
	}
	if total != 5 {
		t.Fatalf("drained %d items, want 5", total)
	}
	// 5 items, MaxBatch 2 -> [2],[2],[1]
	if len(batches) != 3 {
		t.Fatalf("flushed in %d batches, want 3: %v", len(batches), batches)
	}
	if len(batches[0]) != 2 || len(batches[2]) != 1 {
		t.Errorf("unexpected batch sizes: %v", batches)
	}
}

// TestBatcher_AddDropsAfterCancel — Add must not block (nor enqueue)
// once its ctx is cancelled, so a stopped producer never wedges.
func TestBatcher_AddDropsAfterCancel(t *testing.T) {
	b := NewBatcher(BatcherConfig[int]{
		Name: "drop", Interval: time.Hour, MaxBatch: 1, BufferSize: 1,
		Flush: (&recorder{}).flush, Logger: noopLogger{},
	})
	// Fill the (cap-1) channel so the only ready case in Add's select is
	// ctx.Done — otherwise Go could randomly pick the still-open send.
	b.ch <- 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() { b.Add(ctx, 99); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Add blocked after ctx cancel")
	}
	if len(b.ch) != 1 {
		t.Errorf("item enqueued despite full channel + cancelled ctx: ch len = %d", len(b.ch))
	}
}

// TestBatcher_FlushErrorIsSwallowed — a Flush error is logged, the batch
// is dropped, and Run keeps going (does not panic or stop).
func TestBatcher_FlushErrorIsSwallowed(t *testing.T) {
	rec := &recorder{got: make(chan []int, 4), err: errors.New("sink down")}
	b := NewBatcher(BatcherConfig[int]{
		Name: "err", Interval: time.Hour, MaxBatch: 1,
		Flush: rec.flush, Logger: noopLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.Add(ctx, 1)
	_ = await(t, rec.got) // flush ran (and returned an error internally)

	// A second item still flushes -> Run survived the error.
	b.Add(ctx, 2)
	if batch := await(t, rec.got); len(batch) != 1 {
		t.Errorf("second flush = %d, want 1", len(batch))
	}
}

// TestBatcher_EmptyFlushSkips — an interval tick with nothing buffered
// must not call Flush, and neither must the shutdown drain.
func TestBatcher_EmptyFlushSkips(t *testing.T) {
	rec := &recorder{got: make(chan []int, 4)}
	b := NewBatcher(BatcherConfig[int]{
		Name: "empty", Interval: 10 * time.Millisecond, MaxBatch: 100,
		Flush: rec.flush, Logger: noopLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	// Let several ticks pass with no items.
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	if rec.count() != 0 {
		t.Errorf("empty batcher flushed %d items, want 0", rec.count())
	}
}
