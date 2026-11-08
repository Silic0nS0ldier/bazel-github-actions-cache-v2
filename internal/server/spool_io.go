package server

import (
	"context"
	"io"
	"sync/atomic"
)

// maxConcurrentSpoolIO bounds the spool filesystem calls in flight at once.
// Every blocking file syscall pins an OS thread, and Bazel fans uploads out to
// roughly 10k concurrent streams (100 connections of 100 streams each). Left
// unbounded, a large upload burst on a slow runner disk exceeds the Go
// runtime's 10k-thread limit and aborts the process with "fatal error: thread
// exhaustion". A goroutine queued on the gate holds no thread.
const maxConcurrentSpoolIO = 64

// ioGate admits a bounded number of blocking filesystem calls. Slots are held
// only for the duration of each call, never across network reads, so a slow
// client cannot starve other uploads.
type ioGate struct {
	slots  chan struct{}
	active atomic.Int64
	peak   atomic.Int64
}

func newIOGate(limit int) *ioGate {
	return &ioGate{slots: make(chan struct{}, limit)}
}

func (g *ioGate) enter(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	active := g.active.Add(1)
	for {
		peak := g.peak.Load()
		if active <= peak || g.peak.CompareAndSwap(peak, active) {
			return nil
		}
	}
}

func (g *ioGate) exit() {
	g.active.Add(-1)
	<-g.slots
}

// do runs call while holding a slot. Cleanup that must happen regardless of
// cancellation should pass context.Background.
func (g *ioGate) do(ctx context.Context, call func() error) error {
	if err := g.enter(ctx); err != nil {
		return err
	}
	defer g.exit()
	return call()
}

// gatedWriter holds a gate slot for each write to the underlying file.
type gatedWriter struct {
	ctx  context.Context
	gate *ioGate
	w    io.Writer
}

func (w gatedWriter) Write(p []byte) (int, error) {
	if err := w.gate.enter(w.ctx); err != nil {
		return 0, err
	}
	defer w.gate.exit()
	return w.w.Write(p)
}
