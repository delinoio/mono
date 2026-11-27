// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-guest/execlog"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// onFrame handles one frame of the control plane.
func (r *relay) onFrame(c *rconn, f *ownsandv1.DataFrame) {
	if err := datapath.ValidateFrame(f); err != nil {
		if f.GetStreamId() == datapath.ConnectionStreamID {
			// Unknown or malformed connection-level frames are ignored.
			return
		}
		var unknown *datapath.UnknownKindError
		code := ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
		if errors.As(err, &unknown) {
			code = ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED
		}
		r.abortStream(c, f.GetStreamId(), f.GetSandboxId(), code, "unknown or malformed frame")
		return
	}
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_RelayKeepalive:
		if p.RelayKeepalive.GetEchoRequested() {
			_ = c.enqueue(keepaliveFrame(false))
		}
	case *ownsandv1.DataFrame_SupportedFrameKinds:
		r.mu.Lock()
		for _, k := range p.SupportedFrameKinds.GetKinds() {
			c.kinds[k] = true
		}
		r.mu.Unlock()
	case *ownsandv1.DataFrame_RelayStreamOpen:
		r.openStream(c, f, p.RelayStreamOpen)
	case *ownsandv1.DataFrame_RelayStreamClose:
		r.mu.Lock()
		st := c.streams[f.GetStreamId()]
		r.mu.Unlock()
		if st != nil {
			// Releases the output subscription; the operation continues.
			r.finishStream(st, nil, nil)
		}
	case *ownsandv1.DataFrame_Error:
		// The control plane ended the stream with an error frame (for example
		// for a frame kind that it does not know). Like a stream close frame,
		// it releases the output subscription and the operation continues. The
		// agent sends nothing back, because the control plane already closed
		// the stream, and does not mark the grant done, because the frame
		// reports no outcome of the operation.
		r.mu.Lock()
		st := c.streams[f.GetStreamId()]
		r.mu.Unlock()
		if st != nil {
			r.finishStream(st, nil, nil)
		}
	case *ownsandv1.DataFrame_RelayWindowUpdate:
		r.mu.Lock()
		st := c.streams[f.GetStreamId()]
		r.mu.Unlock()
		if st != nil {
			if err := st.send.Add(p.RelayWindowUpdate.GetIncrementBytes()); err != nil {
				r.abortStream(c, st.id, st.g.sandboxID, ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid window update")
			}
		}
	default:
		if datapath.IsRequestKind(f.GetKind()) {
			r.request(c, f)
			return
		}
		r.abortStream(c, f.GetStreamId(), f.GetSandboxId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "frame kind is not sent toward the agent")
	}
}

// abortStream ends a stream with an error frame.
func (r *relay) abortStream(c *rconn, id uint64, sandboxID string, code ownsandv1.ErrorCode, msg string) {
	r.mu.Lock()
	st := c.streams[id]
	if st == nil {
		if id <= c.highest {
			r.mu.Unlock()
			return // The stream already ended or was rejected.
		}
		// A malformed first frame consumes its ID, just like a rejected open.
		c.highest = id
	}
	r.mu.Unlock()
	detail := &ownsandv1.ErrorDetail{Code: code, Message: msg}
	if st == nil {
		r.rejectStream(c, id, sandboxID, detail)
		return
	}
	r.finishStream(st, &ops.Outcome{Terminal: true}, detail)
}

// openStream admits a relay stream open frame for a live grant of the
// current channel, within the stream and credit caps, and sends the first
// receive window.
func (r *relay) openStream(c *rconn, f *ownsandv1.DataFrame, open *ownsandv1.RelayStreamOpen) {
	id := f.GetStreamId()
	now := time.Now()
	r.mu.Lock()
	if id <= c.highest {
		// The control plane never reuses a stream_id; drop the frame.
		r.mu.Unlock()
		return
	}
	c.highest = id
	g := r.grants[open.GetGrantId()]
	live := g != nil && r.ch != nil && g.ch.Generation() == c.gen && c.gen == r.ch.Generation() &&
		g.sandboxID == f.GetSandboxId() && g.admittable(now, r.d.timing.AdmissionTolerance)
	if !live {
		r.mu.Unlock()
		r.d.logger.Info("relay stream rejected: no live grant", "grant_id", open.GetGrantId(), "stream_id", id)
		r.rejectStream(c, id, f.GetSandboxId(), &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID})
		return
	}
	if len(g.streams) >= r.limits.MaxStreamsPerGrant || len(c.streams) >= r.limits.MaxStreamsPerConnection ||
		c.credit+r.limits.StreamReceiveWindow > r.limits.MaxConnectionCredit {
		r.mu.Unlock()
		r.d.logger.Warn("relay stream rejected at capacity", "grant_id", g.id, "stream_id", id)
		r.rejectStream(c, id, g.sandboxID, &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: "agent relay stream or credit capacity reached"})
		return
	}
	sendWin, err := datapath.NewSendWindow(open.GetInitialWindowBytes())
	if err != nil {
		r.mu.Unlock()
		r.rejectStream(c, id, g.sandboxID, &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Message: "invalid initial window"})
		return
	}
	recvWin, _ := datapath.NewReceiveWindow(r.limits.StreamReceiveWindow)
	ctx, cancel := context.WithCancel(c.ctx)
	st := &rstream{id: id, g: g, conn: c, send: sendWin, recv: recvWin, ctx: ctx, cancel: cancel}
	c.streams[id] = st
	c.credit += r.limits.StreamReceiveWindow
	g.streams[st] = true
	r.mu.Unlock()
	_ = r.enqueueStream(st.ctx, st, datapath.MustNewFrame(id, g.sandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: recvWin.InitialWindow()}))
}

// request handles the first request frame of a stream.
func (r *relay) request(c *rconn, f *ownsandv1.DataFrame) {
	r.mu.Lock()
	st := c.streams[f.GetStreamId()]
	if st == nil {
		r.mu.Unlock()
		return
	}
	started := st.started
	st.started = true
	g := st.g
	r.mu.Unlock()
	if err := st.recv.Receive(datapath.FrameCost(f)); err != nil {
		r.abortStream(c, st.id, g.sandboxID, ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "request frame exceeds the granted window")
		return
	}
	if started {
		r.abortStream(c, st.id, g.sandboxID, ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "a relay stream carries one request")
		return
	}
	r.mu.Lock()
	_, held := r.grants[g.id]
	admittable := held && g.admittable(time.Now(), r.d.timing.AdmissionTolerance)
	r.mu.Unlock()
	if !admittable {
		r.finishStream(st, nil, &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID})
		return
	}
	go func() {
		out := r.d.opts.Dispatcher.Handle(st.ctx, ops.Request{
			Frame:     f,
			SandboxID: g.sandboxID,
			Grant:     &ops.GrantCheck{Kind: g.kind, OperationID: g.opID, Digest: g.digest},
			OnAccepted: func() {
				// The dispatcher accepted a request under the grant: only now
				// does the grant count as used (see grant.opened). The notice
				// waits on the stream context, so a close frame ends the wait.
				r.mu.Lock()
				g.opened = true
				r.mu.Unlock()
				_ = r.enqueueStream(st.ctx, st, datapath.MustNewFrame(st.id, g.sandboxID, &ownsandv1.RequestAccepted{}))
			},
		}, relaySink{r, st})
		_, _ = st.recv.Release(datapath.FrameCost(f))
		r.finishStream(st, &out, nil)
	}()
}

type relaySink struct {
	r  *relay
	st *rstream
}

// Send sends payload frames only within the credit window that the control
// plane granted for the stream. A wait for a free outbound slot ends with
// ctx, so a stream close frame releases an operation that is blocked on a full
// queue instead of letting it queue a frame for the closed stream later.
func (k relaySink) Send(ctx context.Context, f *ownsandv1.DataFrame) error {
	if cost := datapath.FrameCost(f); cost > 0 {
		if err := k.st.send.Consume(ctx, cost); err != nil {
			return err
		}
	}
	return k.r.enqueueStream(ctx, k.st, f)
}

// enqueueStream admits a frame under the same lock as stream termination.
// When the queue is full, the sender waits without the lock and rechecks
// admission before it offers the frame again.
func (r *relay) enqueueStream(ctx context.Context, st *rstream, f *ownsandv1.DataFrame) error {
	c := st.conn
	final := isRelayFinal(f)
	for {
		r.mu.Lock()
		if err := st.ctx.Err(); err != nil {
			r.mu.Unlock()
			return err
		}
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return err
		}
		if st.terminalQueued {
			r.mu.Unlock()
			return context.Canceled
		}
		if c.tryEnqueue(f) {
			st.terminalQueued = final
			r.mu.Unlock()
			return nil
		}
		// The writer takes this lock to publish free space, so a dequeue
		// after tryEnqueue fails cannot be lost.
		if c.outSpace == nil {
			c.outSpace = make(chan struct{})
		}
		space := c.outSpace
		r.mu.Unlock()
		select {
		case <-space:
		case <-st.ctx.Done():
			return st.ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// isRelayFinal identifies the last response of an operation on this stream.
func isRelayFinal(f *ownsandv1.DataFrame) bool {
	return f.GetError() != nil || execlog.IsFinal(f.GetExecEvent()) || f.GetExecEvent().GetPaused() != nil ||
		f.GetFileReadEvent().GetEnd() != nil || f.GetFileWriteResult() != nil
}

// finishStream closes admission and offers a terminal frame, if any, as
// one action. out is the dispatcher outcome or a relay request rejection.
func (r *relay) finishStream(st *rstream, out *ops.Outcome, terminal proto.Message) {
	r.mu.Lock()
	if st.ended {
		r.mu.Unlock()
		return
	}
	st.ended = true
	var fallback *ownsandv1.DataFrame
	if terminal != nil {
		if st.terminalQueued {
			out = nil // The dispatcher already sent a final response.
		} else {
			f := datapath.MustNewFrame(st.id, st.g.sandboxID, terminal)
			if st.conn.offerTerminal(f) {
				fallback = f
			}
		}
	}
	st.cancel()
	if st.conn.streams[st.id] == st {
		delete(st.conn.streams, st.id)
		st.conn.credit -= r.limits.StreamReceiveWindow
	}
	g := st.g
	delete(g.streams, st)
	g.idleSince = time.Now()
	if out != nil && (out.Finished || out.Terminal) {
		g.done = true
	}
	r.mu.Unlock()
	if fallback != nil {
		r.logTerminalFallback(st.conn, fallback, g.id)
	}
	r.poke()
}

// endGrantStreams ends every open stream of a grant with a session-end
// frame. It never waits on the outbound queue: revoke and the grant
// replacement run on the agent channel receive loop, and the deadline sweep
// runs on the housekeeping goroutine, so a wait on a stalled writer would
// block the agent channel or every grant sweep. A connection that cannot
// take the terminal frame is closed instead, and the reconnect rules of the
// relay loop apply.
func (r *relay) endGrantStreams(g *grant, reason ownsandv1.SessionEndReason) {
	r.mu.Lock()
	streams := make([]*rstream, 0, len(g.streams))
	for st := range g.streams {
		streams = append(streams, st)
	}
	r.mu.Unlock()
	for _, st := range streams {
		r.finishStream(st, nil, &ownsandv1.SessionEnd{Reason: reason})
	}
}

// offerTerminal cancels the connection when it cannot queue a terminal frame
// immediately. It reports whether the queue was full. A caller ending an
// admitted stream holds relay.mu so a blocked sender cannot overtake it.
func (c *rconn) offerTerminal(f *ownsandv1.DataFrame) bool {
	if c.ctx.Err() != nil || c.tryEnqueue(f) {
		return false
	}
	c.cancel()
	return true
}

// rejectStream sends a terminal response for an ID without admitted state.
// The receiver already consumed the ID in c.highest.
func (r *relay) rejectStream(c *rconn, id uint64, sandboxID string, terminal proto.Message) {
	f := datapath.MustNewFrame(id, sandboxID, terminal)
	if c.offerTerminal(f) {
		r.logTerminalFallback(c, f, "")
	}
}

func (r *relay) logTerminalFallback(c *rconn, f *ownsandv1.DataFrame, grantID string) {
	if !c.queueFullLogged.CompareAndSwap(false, true) {
		return
	}
	if end := f.GetSessionEnd(); end != nil {
		r.d.logger.Warn("relay outbound queue is full; closing the connection instead of queueing a session end",
			"generation", c.gen, "termination_reason", end.GetReason().String(), "grant_id", grantID)
		return
	}
	r.d.logger.Warn("relay outbound queue is full; closing the connection instead of queueing a stream error",
		"generation", c.gen, "error_code", f.GetError().GetCode().String(), "grant_id", grantID)
}

// revoke ends a grant at once.
func (r *relay) revoke(grantID string) {
	r.mu.Lock()
	g := r.grants[grantID]
	delete(r.grants, grantID)
	r.mu.Unlock()
	if g != nil {
		r.endGrantStreams(g, ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
		r.d.logger.Info("relay grant revoked", "grant_id", grantID)
	}
}

// channelLost ends every grant of the channel with AGENT_CHANNEL_LOST and
// closes the relay connection of the channel.
func (r *relay) channelLost(ch interface{ Generation() uint64 }) {
	r.mu.Lock()
	var lost []*grant
	for id, g := range r.grants {
		if g.ch.Generation() == ch.Generation() {
			lost = append(lost, g)
			delete(r.grants, id)
		}
	}
	c := r.conn
	r.mu.Unlock()
	for _, g := range lost {
		r.endGrantStreams(g, ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
	if c != nil && c.gen == ch.Generation() {
		// The session-end frames leave before the connection closes.
		go c.closeAfterFlush()
	}
}

// housekeeping releases grants and ends streams at their deadline.
func (r *relay) housekeeping() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
		r.sweep(time.Now())
	}
}

// expiredGrantReleasable reports whether a grant without an open stream is
// released because of its expiry. A grant whose last stream ended at or after
// the expiry is released at once, as soon as that stream ends. A grant that
// never admitted a stream, or whose last stream ended before the expiry, can
// still admit a first request frame until the expiry plus the admission
// tolerance (see grant.admittable), so it stays until then; releasing it at
// the bare expiry would make the control plane's own admittable check accept
// a stream that the agent no longer knows. The caller holds r.mu.
func expiredGrantReleasable(g *grant, now time.Time, tolerance time.Duration) bool {
	expiry := g.received.Add(g.expiresIn)
	if !now.Before(expiry.Add(tolerance)) {
		return true
	}
	return !now.Before(expiry) && !g.idleSince.Before(expiry)
}

func (r *relay) sweep(now time.Time) {
	// An exec keeps running after its caller closed the stream (for example
	// to attach again later). Its grant is released as soon as the exec
	// ends, not only after the idle time. The dispatcher is asked outside
	// r.mu; only grants that admitted a stream qualify, so a grant issued
	// for an AttachExec of an already ended exec is not released before the
	// caller opens its stream.
	r.mu.Lock()
	var execIDs []string
	for _, g := range r.grants {
		if len(g.streams) == 0 && g.opened && !g.done && g.execGrant() {
			execIDs = append(execIDs, g.opID)
		}
	}
	r.mu.Unlock()
	endedExecs := map[string]bool{}
	for _, id := range execIDs {
		if r.d.opts.Dispatcher.ExecEnded(id) {
			endedExecs[id] = true
		}
	}

	var expired, released []*grant
	r.mu.Lock()
	for id, g := range r.grants {
		switch {
		case !now.Before(g.received.Add(g.deadlineIn)):
			// The stream deadline ends the open streams without tolerance.
			delete(r.grants, id)
			expired = append(expired, g)
		case len(g.streams) == 0 && (g.done || (g.opened && g.execGrant() && endedExecs[g.opID]) ||
			now.Sub(g.idleSince) >= r.d.timing.GrantIdle ||
			expiredGrantReleasable(g, now, r.d.timing.AdmissionTolerance)):
			delete(r.grants, id)
			released = append(released, g)
		}
	}
	r.mu.Unlock()
	for _, g := range expired {
		r.endGrantStreams(g, ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
		r.d.logger.Info("relay grant reached its stream deadline", "grant_id", g.id)
	}
	for _, g := range released {
		g.ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantRelease{RelayGrantRelease: &ownsandv1.RelayGrantRelease{GrantId: g.id}}})
		r.d.logger.Info("relay grant released", "grant_id", g.id, "operation_done", g.done)
	}
}

func (r *relay) close() {
	r.cancel()
}
