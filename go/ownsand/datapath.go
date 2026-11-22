// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// errHandleClosed is returned by a peer-to-peer operation on a handle that
// was closed, and by one whose session setup was still running at Close.
var errHandleClosed = errors.New("ownsand: sandbox handle closed")

// dataPath selects the transport of one sandbox handle: one peer-to-peer
// session shared by all operations, or the relay after an ICE failure or a
// session that could not open.
type dataPath struct {
	s *Sandbox

	// openSlot (capacity 1) serializes session setup. It is a channel and
	// not a mutex, so an operation that waits for the setup of another
	// operation can stop waiting when its own context ends.
	openSlot chan struct{}
	// done is closed by close. It wakes the operations that wait for the
	// setup slot and cancels the setup that is in flight.
	done chan struct{}

	mu     sync.Mutex
	sess   *p2pSession
	relay  bool
	inUse  ownsandv1.DataPathTransport
	closed bool
}

func newDataPath(s *Sandbox) *dataPath {
	return &dataPath{s: s, openSlot: make(chan struct{}, 1), done: make(chan struct{})}
}

// acquireOpen waits for the setup slot. It returns the context error of the
// waiting operation as soon as ctx ends, and errHandleClosed when the handle
// closes first, without waiting for the setup that holds the slot.
func (d *dataPath) acquireOpen(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case d.openSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
		return errHandleClosed
	}
}

func (d *dataPath) releaseOpen() { <-d.openSlot }

// route returns the session to use, or nil for the relay. It opens a new
// session only when an operation needs one, so an idle handle never keeps
// the sandbox active. A closed handle rejects a peer-to-peer operation with
// errHandleClosed before it calls the control plane; the relay-only
// transport never reaches this check.
//
// One setup at a time runs for the handle. An operation that needs a session
// while another operation sets it up waits for that setup, and stops waiting
// (with its own context error) when its context ends; the setup continues
// for the other operations. When the operation that runs the setup ends
// first (its context ended, or the handle closed), the waiting operations
// run the setup again, one at a time.
func (d *dataPath) route(ctx context.Context) (*p2pSession, error) {
	c := d.s.c
	if c.transport == TransportRelay {
		d.setInUse(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
		return nil, nil
	}
	if sess, relay, err := d.lookup(); sess != nil || relay || err != nil {
		return sess, err
	}

	if err := d.acquireOpen(ctx); err != nil {
		return nil, err
	}
	defer d.releaseOpen()
	if sess, relay, err := d.lookup(); sess != nil || relay || err != nil {
		return sess, err
	}

	// Close cancels the setup that is in flight, so a closed handle does not
	// wait for a setup that nobody can use.
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-d.done:
			cancel(errHandleClosed)
		case <-stop:
		}
	}()

	start := time.Now()
	sess, err := openP2PSession(sctx, c, d.s.org, d.s.id)
	if c.p2p.setupDone != nil {
		c.p2p.setupDone()
	}
	// A session that the setup produced after Close is closed and never
	// installed.
	d.mu.Lock()
	closed := d.closed
	if err == nil && !closed {
		d.sess = sess
		d.inUse = ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER
	}
	d.mu.Unlock()
	if closed {
		if err == nil {
			sess.close()
		}
		return nil, errHandleClosed
	}
	if err == nil {
		c.logger.DebugContext(ctx, "ownsand data path session opened", "sandbox_id", d.s.id, "session_id", sess.sessionID, "transport", "peer-to-peer", "duration", time.Since(start))
		return sess, nil
	}
	if cerr := callerDeadlineEnded(ctx, err, c.timing.callerDeadlineSkew); cerr != nil {
		// The caller's context ended first; it stays authoritative and
		// starts no relay recovery.
		return nil, cerr
	}
	if IsBillingError(err) {
		// Billing enforcement rejected GetIceServers or OpenSession. The
		// relay is rejected for the same reason, so the error ends the
		// operation instead of a fall back to the relay.
		c.logger.DebugContext(ctx, "ownsand data path rejected by billing", "sandbox_id", d.s.id, "billing_reason", BillingReasonOf(err).String())
		return nil, err
	}
	var openErr *errP2POpen
	if !errors.As(err, &openErr) {
		// OpenSession or GetIceServers failed. An authorization error ends
		// the operations; other errors mean the session cannot open.
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeCanceled:
			return nil, err
		case connect.CodeFailedPrecondition:
			// The sandbox is not RUNNING (for example PAUSED, where an
			// AttachExec still works on the relay). Use the relay for this
			// attempt without moving the handle.
			if c.transport == TransportPeerToPeer {
				return nil, err
			}
			c.logger.DebugContext(ctx, "ownsand data path session not open for this sandbox state; using the relay once", "sandbox_id", d.s.id)
			return nil, nil
		}
	}
	if c.transport == TransportPeerToPeer {
		return nil, err
	}
	c.logger.DebugContext(ctx, "ownsand data path falls back to the relay", "sandbox_id", d.s.id, "code", connect.CodeOf(err).String())
	d.mu.Lock()
	d.relay = true
	d.inUse = ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY
	d.mu.Unlock()
	return nil, nil
}

// callerDeadlineEnded returns the error of the caller's context when the
// caller's deadline ended a setup step, and nil otherwise. Besides the context
// error itself it covers a transport-level failure of the control plane that
// arrives within skew of the caller's deadline while the context still
// reports no error. Connect sends the time left to the caller's deadline as a
// timeout header, so the server can answer deadline_exceeded before the
// caller's own timer fires (more often under CPU load), and a connection that
// is cut at that moment fails the same way. Such a failure is the caller's
// deadline and not a setup failure, so it must not start relay recovery. The
// call then waits for the context to end (at most skew) so that the context
// error is authoritative. Authorization and argument errors keep their own
// handling.
func callerDeadlineEnded(ctx context.Context, err error, skew time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch connect.CodeOf(err) {
	case connect.CodeDeadlineExceeded, connect.CodeCanceled, connect.CodeUnavailable, connect.CodeUnknown:
	default:
		return nil
	}
	var openErr *errP2POpen
	if errors.As(err, &openErr) {
		// The setup window, not the caller, ended or failed the setup.
		return nil
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > skew {
		return nil
	}
	timer := time.NewTimer(skew)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return context.DeadlineExceeded
}

// sessionFailed handles a session that failed during an operation: the
// handle moves to the relay unless the transport is forced.
func (d *dataPath) sessionFailed(sess *p2pSession) {
	sess.close()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == sess {
		d.sess = nil
	}
	if d.s.c.transport != TransportPeerToPeer && !d.relay {
		d.relay = true
		d.inUse = ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY
		d.s.c.logger.Debug("ownsand data path falls back to the relay after a session failure", "sandbox_id", d.s.id)
	}
}

// sessionEnded forgets a session that the agent ended; the next operation
// opens a new one.
func (d *dataPath) sessionEnded(sess *p2pSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == sess {
		d.sess = nil
	}
}

func (d *dataPath) setInUse(t ownsandv1.DataPathTransport) {
	d.mu.Lock()
	d.inUse = t
	d.mu.Unlock()
}

func (d *dataPath) current() ownsandv1.DataPathTransport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inUse
}

// lookup returns the installed session, or reports that the handle uses the
// relay, or that it is closed. It returns all zero values when a session must
// open.
func (d *dataPath) lookup() (sess *p2pSession, relay bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.closed:
		return nil, false, errHandleClosed
	case d.relay:
		return nil, true, nil
	case d.sess != nil && !d.sess.isDone():
		return d.sess, false, nil
	}
	return nil, false, nil
}

// close marks the handle closed, closes the installed session and cancels a
// setup in flight, whose session (if it still opens) is closed instead of
// installed. It is idempotent.
func (d *dataPath) close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.done)
	}
	sess := d.sess
	d.sess = nil
	d.mu.Unlock()
	if sess != nil {
		sess.close()
	}
}

// handleSessionError reports whether err ended the session (true) and
// updates the handle: a session end, retirement or caller close only forgets
// the session, a failure moves the handle to the relay.
func (d *dataPath) handleSessionError(sess *p2pSession, err error) bool {
	var ended *errSessionEnded
	if errors.As(err, &ended) {
		d.s.c.logger.Debug("ownsand data path session ended", "sandbox_id", d.s.id, "reason", ended.reason.String())
		d.sessionEnded(sess)
		return true
	}
	var retired *errSessionRetired
	if errors.As(err, &retired) {
		d.s.c.logger.Debug("ownsand data path session retired after an interrupted frame", "sandbox_id", d.s.id)
		d.sessionEnded(sess)
		return true
	}
	var closedByCaller *errSessionClosedByCaller
	if errors.As(err, &closedByCaller) {
		// The caller closed the handle, which is not a channel failure: the
		// handle keeps its transport and the operation continues to
		// errHandleClosed at its next route call.
		d.sessionEnded(sess)
		return true
	}
	var failed *errP2PFailed
	if errors.As(err, &failed) {
		d.sessionFailed(sess)
		return true
	}
	return false
}

// relayGrant is the relay grant of one operation. The SDK uses a grant only
// for its operation and renews it only between streams, with
// replaces_grant_id, never while a stream of the old grant is open.
type relayGrant struct {
	s      *Sandbox
	kind   ownsandv1.RelayOperationKind
	opID   string
	digest []byte

	grantID   string
	expiresAt time.Time // on the monotonic clock, from expires_in at receipt
	idleSince time.Time // receipt, or the end of the last stream
	stale     bool      // discarded after a failure
}

func newRelayGrant(s *Sandbox, kind ownsandv1.RelayOperationKind, opID string, digest []byte) *relayGrant {
	return &relayGrant{s: s, kind: kind, opID: opID, digest: digest}
}

// ensure makes sure the grant lives at least need longer and that the agent
// cannot have released it as idle. rec is the operation's recovery window
// when the call is part of recovery.
func (g *relayGrant) ensure(ctx context.Context, need time.Duration, rec *recovery) error {
	t := g.s.c.timing
	if g.grantID != "" && !g.stale && time.Until(g.expiresAt) > need && time.Since(g.idleSince) < t.grantIdle {
		return nil
	}
	return g.authorize(ctx, rec)
}

// discard marks the grant for renewal after a failure.
func (g *relayGrant) discard() { g.stale = true }

// streamEnded records the end of a stream of the grant.
func (g *relayGrant) streamEnded() { g.idleSince = time.Now() }

// authorize calls AuthorizeRelay with one authorization_id and retries it
// on unavailable, resource_exhausted (after the retry delay) and transport
// interruptions within the recovery window (or a fresh 60-second window).
// A renewal passes the grant that it replaces.
func (g *relayGrant) authorize(ctx context.Context, rec *recovery) error {
	c := g.s.c
	authID, err := uuidv7.NewString()
	if err != nil {
		return err
	}
	req := &ownsandv1.AuthorizeRelayRequest{
		OrganizationId:  g.s.org,
		SandboxId:       g.s.id,
		OperationKind:   g.kind,
		OperationId:     g.opID,
		RequestDigest:   g.digest,
		AuthorizationId: authID,
	}
	if g.grantID != "" {
		replaces := g.grantID
		req.ReplacesGrantId = &replaces
	}
	// Inside relay recovery the retries share the operation's window;
	// otherwise (startup or a renewal before the next stream) they get their
	// own 60-second window, which starts with the first attempt.
	var window *recovery
	if rec != nil && rec.inRecovery() {
		window = rec
	} else {
		window = newRecovery(c.timing.recoveryWindow)
		window.begin()
	}
	for {
		// Bound each attempt by the time left in the window: a response
		// that stalls on an open connection must not outlive the window,
		// because the default HTTP client has no overall timeout and the
		// stall detector covers only relay server streams. A timer cancels
		// the attempt instead of a context deadline, so Connect does not
		// send the bound as a request timeout and the server cannot answer
		// it first with a deadline_exceeded status, which is terminal.
		left := window.remaining()
		if left <= 0 {
			if err := window.err(); err != nil {
				return err
			}
			return errAuthorizeDeadline
		}
		actx, cancel := context.WithCancelCause(ctx)
		timer := time.AfterFunc(left, func() { cancel(errAuthorizeDeadline) })
		res, err := c.dataPath.AuthorizeRelay(actx, connect.NewRequest(proto.Clone(req).(*ownsandv1.AuthorizeRelayRequest)))
		timer.Stop()
		if err != nil && ctx.Err() == nil && errors.Is(context.Cause(actx), errAuthorizeDeadline) {
			err = errAuthorizeDeadline
		}
		cancel(context.Canceled)
		if err == nil {
			now := time.Now()
			g.grantID = res.Msg.GetGrantId()
			g.expiresAt = now.Add(res.Msg.GetExpiresIn().AsDuration())
			g.idleSince = now
			g.stale = false
			c.logger.DebugContext(ctx, "ownsand relay grant", "sandbox_id", g.s.id, "operation_id", g.opID, "grant_id", g.grantID, "renewal", req.ReplacesGrantId != nil)
			return nil
		}
		kind, delay := classify(ctx, err)
		if kind == failTerminal || kind == failDeadline {
			return err
		}
		window.fail(err)
		if berr := window.backoff(ctx, c.timing, delay); berr != nil {
			return berr
		}
	}
}

// relayRequest wraps msg with the grant header.
func relayRequest[T any](g *relayGrant, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set(RelayGrantHeader, g.grantID)
	return req
}

// stallContext returns a context for one relay server stream that is
// cancelled with errStall when the response body receives no byte for the
// stall timeout. A positive budget also cancels it with errRecoveryDeadline
// when the budget passes before the caller calls disarm, which the operation
// does when the agent accepts the stream or its progress ends the recovery
// window. A zero budget arms no such timer. The timer, not a context
// deadline, cancels the stream, so a blocked Receive and keepalive-only
// traffic obey the budget and Connect sends no request timeout.
func (c *Client) stallContext(ctx context.Context, budget time.Duration) (actx context.Context, disarm func(), stop func()) {
	actx, cancel := context.WithCancelCause(ctx)
	a := newActivity()
	actx = withActivity(actx, a)
	var timer *time.Timer
	if budget > 0 {
		timer = time.AfterFunc(budget, func() { cancel(errRecoveryDeadline) })
	}
	disarm = func() {
		if timer != nil {
			timer.Stop()
		}
	}
	quit := make(chan struct{})
	go func() {
		tick := max(c.timing.streamStall/20, time.Millisecond)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-actx.Done():
				return
			case <-t.C:
				if a.idle() >= c.timing.streamStall {
					cancel(errStall)
					return
				}
			}
		}
	}()
	return actx, disarm, func() {
		disarm()
		close(quit)
		cancel(context.Canceled)
	}
}

// streamErr turns the error of a stream that the SDK cancelled into the
// cause: errStall for a stalled stream, and for a continuation that passed
// its recovery budget before acceptance the deadline error of rec.
func streamErr(actx context.Context, err error, rec *recovery) error {
	if err == nil {
		return nil
	}
	switch cause := context.Cause(actx); {
	case errors.Is(cause, errStall):
		return errStall
	case errors.Is(cause, errRecoveryDeadline):
		return rec.deadlineErr()
	}
	return err
}
