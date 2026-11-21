// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func (r *relay) sendFrame(c *rconn, id uint64, sandboxID string, payload proto.Message) {
	_ = c.enqueue(datapath.MustNewFrame(id, sandboxID, payload))
}

func (r *relay) errorFrame(c *rconn, id uint64, sandboxID string, code ownsandv1.ErrorCode, msg string) {
	r.sendFrame(c, id, sandboxID, &ownsandv1.ErrorDetail{Code: code, Message: msg})
}

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
			r.finishStream(st, nil)
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
	r.mu.Unlock()
	if st != nil {
		sandboxID = st.g.sandboxID
	}
	r.errorFrame(c, id, sandboxID, code, msg)
	if st != nil {
		r.finishStream(st, &ops.Outcome{Terminal: true})
	}
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
		r.sendFrame(c, id, f.GetSandboxId(), &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID})
		return
	}
	if len(g.streams) >= r.limits.MaxStreamsPerGrant || len(c.streams) >= r.limits.MaxStreamsPerConnection ||
		c.credit+r.limits.StreamReceiveWindow > r.limits.MaxConnectionCredit {
		r.mu.Unlock()
		r.d.logger.Warn("relay stream rejected at capacity", "grant_id", g.id, "stream_id", id)
		r.errorFrame(c, id, g.sandboxID, ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "agent relay stream or credit capacity reached")
		return
	}
	sendWin, err := datapath.NewSendWindow(open.GetInitialWindowBytes())
	if err != nil {
		r.mu.Unlock()
		r.errorFrame(c, id, g.sandboxID, ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid initial window")
		return
	}
	recvWin, _ := datapath.NewReceiveWindow(r.limits.StreamReceiveWindow)
	ctx, cancel := context.WithCancel(c.ctx)
	st := &rstream{id: id, g: g, conn: c, send: sendWin, recv: recvWin, ctx: ctx, cancel: cancel}
	c.streams[id] = st
	c.credit += r.limits.StreamReceiveWindow
	g.streams[st] = true
	r.mu.Unlock()
	r.sendFrame(c, id, g.sandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: recvWin.InitialWindow()})
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
		r.sendFrame(c, st.id, g.sandboxID, &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID})
		r.finishStream(st, &ops.Outcome{})
		return
	}
	go func() {
		out := r.d.opts.Dispatcher.Handle(st.ctx, ops.Request{
			Frame:     f,
			SandboxID: g.sandboxID,
			Grant:     &ops.GrantCheck{Kind: g.kind, OperationID: g.opID, Digest: g.digest},
			OnAccepted: func() {
				r.sendFrame(c, st.id, g.sandboxID, &ownsandv1.RequestAccepted{})
			},
		}, relaySink{st})
		_, _ = st.recv.Release(datapath.FrameCost(f))
		r.finishStream(st, &out)
	}()
}

type relaySink struct{ st *rstream }

// Send sends payload frames only within the credit window that the control
// plane granted for the stream.
func (k relaySink) Send(ctx context.Context, f *ownsandv1.DataFrame) error {
	if cost := datapath.FrameCost(f); cost > 0 {
		if err := k.st.send.Consume(ctx, cost); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return k.st.conn.enqueue(f)
}

// finishStream removes a stream. out is the operation outcome when the
// agent ended the stream after a request.
func (r *relay) finishStream(st *rstream, out *ops.Outcome) {
	r.mu.Lock()
	if st.ended {
		r.mu.Unlock()
		return
	}
	st.ended = true
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
	st.cancel()
	r.poke()
}

// endGrantStreams ends every open stream of a grant with a session-end
// frame.
func (r *relay) endGrantStreams(g *grant, reason ownsandv1.SessionEndReason) {
	r.mu.Lock()
	streams := make([]*rstream, 0, len(g.streams))
	for st := range g.streams {
		streams = append(streams, st)
	}
	r.mu.Unlock()
	for _, st := range streams {
		r.sendFrame(st.conn, st.id, g.sandboxID, &ownsandv1.SessionEnd{Reason: reason})
		r.finishStream(st, nil)
	}
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

func (r *relay) sweep(now time.Time) {
	var expired, released []*grant
	r.mu.Lock()
	for id, g := range r.grants {
		switch {
		case !now.Before(g.received.Add(g.deadlineIn)):
			// The stream deadline ends the open streams without tolerance.
			delete(r.grants, id)
			expired = append(expired, g)
		case len(g.streams) == 0 && (g.done ||
			now.Sub(g.idleSince) >= r.d.timing.GrantIdle ||
			!now.Before(g.received.Add(g.expiresIn))):
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
