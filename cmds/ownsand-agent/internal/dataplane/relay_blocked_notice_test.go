// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"sync/atomic"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
)

// waitProbeContext is a stream context that reports the first Done call after
// arm. The relay code that waits for an outbound slot (enqueueCtx) evaluates
// ctx.Done() when it enters its select, after ctx.Err() returned nil. A test
// therefore learns, without timing assumptions or goroutine stack parsing,
// that the wait on the stream context has begun. A close that follows cancels
// the wrapped context, which the select observes whether it is already parked
// or has not parked yet.
type waitProbeContext struct {
	context.Context
	armed  atomic.Bool
	waited chan struct{}
}

func newWaitProbeContext(parent context.Context) *waitProbeContext {
	return &waitProbeContext{Context: parent, waited: make(chan struct{})}
}

// arm makes the next Done call close the waited channel.
func (c *waitProbeContext) arm() { c.armed.Store(true) }

func (c *waitProbeContext) Done() <-chan struct{} {
	if c.armed.CompareAndSwap(true, false) {
		close(c.waited)
	}
	return c.Context.Done()
}

// acceptNoticeDispatcher is a dataplane Dispatcher that accepts every request
// at once and then waits for the stream context, like an operation that
// continues until its stream closes. It arms the probe context right before
// it calls OnAccepted, so the first Done call belongs to the notice's wait.
type acceptNoticeDispatcher struct {
	probe *waitProbeContext
	// accepted closes when OnAccepted returned.
	accepted chan struct{}
}

func (d *acceptNoticeDispatcher) Handle(ctx context.Context, req ops.Request, _ ops.Sink) ops.Outcome {
	d.probe.arm()
	req.OnAccepted()
	close(d.accepted)
	<-ctx.Done()
	return ops.Outcome{}
}

func (d *acceptNoticeDispatcher) ExecEnded(string) bool { return false }
