package ownsand

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Data path timing rules of the SDK contract.
const (
	// recoveryWindow is the length of one relay recovery window, of the
	// authorization retry window and of the WriteFile chunk budget.
	recoveryWindow = 60 * time.Second
	// relayStreamLimit is the control plane's limit of one relay server stream.
	relayStreamLimit = 14 * time.Minute
	// writeCallLimit is the control plane's limit of one relay WriteFile call.
	writeCallLimit = 5 * time.Minute
	// grantMargin is added to the stream or call limit when the SDK checks
	// whether a grant outlives the next stream.
	grantMargin = 30 * time.Second
	// grantIdleRelease is how long a grant may have no open stream before
	// the agent may release it.
	grantIdleRelease = 60 * time.Second
	// writeAttemptDeadline is the SDK-side deadline of one WriteFile attempt.
	writeAttemptDeadline = writeCallLimit + 30*time.Second
	// backoffInitial and backoffMax bound the retry back-off.
	backoffInitial = 250 * time.Millisecond
	backoffMax     = 5 * time.Second
)

// errWriteDeadline is the cancel cause of a WriteFile attempt that passed
// its SDK-side deadline.
var errWriteDeadline = errors.New("WriteFile attempt got no response within 5 minutes 30 seconds")

// failure classifies an error of an RPC or of a data channel request.
type failure struct {
	// transport is true for a transport interruption: the call ended
	// without a valid terminal status (or the SDK cancelled the attempt).
	transport bool
	// remote is true for a terminal status that the server or agent sent.
	remote bool
	code   connect.Code
	// retryDelay is the delay of a resource_exhausted RetryDetail.
	retryDelay time.Duration
	// sessionEnd is the SessionEndReason of an unavailable error detail.
	sessionEnd ownsandv1.SessionEndReason
	// canceled is true when the caller's context ended.
	canceled bool
}

// remoteError is a terminal error that the agent sent in an error frame on
// the peer-to-peer data channel.
type remoteError struct {
	code connect.Code
	msg  string
}

func (e *remoteError) Error() string { return fmt.Sprintf("%s: %s", e.code, e.msg) }

func classify(ctx context.Context, err error) failure {
	return classifyAttempt(ctx, nil, err)
}

// classifyAttempt classifies err of an attempt whose context is attempt.
func classifyAttempt(ctx, attempt context.Context, err error) failure {
	if ctx.Err() != nil {
		return failure{canceled: true, code: connect.CodeCanceled}
	}
	if attempt != nil {
		if cause := context.Cause(attempt); errors.Is(cause, errStreamIdle) || errors.Is(cause, errWriteDeadline) {
			return failure{transport: true, code: connect.CodeUnavailable}
		}
	}
	var re *remoteError
	if errors.As(err, &re) {
		return failure{remote: true, code: re.code}
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return failure{code: connect.CodeUnknown}
	}
	if !connect.IsWireError(ce) {
		// The call ended without a terminal status from the server, for
		// example a closed connection or a stream without its end message.
		return failure{transport: true, code: ce.Code()}
	}
	f := failure{remote: true, code: ce.Code()}
	for _, d := range ce.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		switch m := v.(type) {
		case *ownsandv1.RetryDetail:
			f.retryDelay = m.GetRetryDelay().AsDuration()
		case *ownsandv1.SessionEndDetail:
			f.sessionEnd = m.GetReason()
		}
	}
	return f
}

// retryable reports whether the relay recovery rules retry the failure.
func (f failure) retryable(write bool) bool {
	if f.canceled {
		return false
	}
	if f.transport {
		return true
	}
	if !f.remote {
		return false
	}
	switch f.code {
	case connect.CodeUnavailable, connect.CodeResourceExhausted:
		return true
	case connect.CodeDeadlineExceeded:
		return write
	}
	return false
}

// backoff is an exponential back-off.
type backoff struct{ cur time.Duration }

func newBackoff() *backoff { return &backoff{} }

// next returns the next delay, at least minDelay.
func (b *backoff) next(minDelay time.Duration) time.Duration {
	if b.cur == 0 {
		b.cur = backoffInitial
	} else {
		b.cur = min(b.cur*2, backoffMax)
	}
	return max(b.cur, minDelay)
}

// window is a 60-second recovery window whose clock can pause.
type window struct {
	clk       clock.Clock
	active    bool
	running   bool
	remaining time.Duration
	since     time.Time
}

func newWindow(clk clock.Clock) *window { return &window{clk: clk} }

// start opens the window when it is not open, and resumes its clock.
func (w *window) start() {
	if !w.active {
		w.active = true
		w.remaining = recoveryWindow
		w.since = w.clk.Now()
		w.running = true
		return
	}
	w.resume()
}

func (w *window) left() time.Duration {
	if !w.active {
		return recoveryWindow
	}
	if w.running {
		return w.remaining - w.clk.Now().Sub(w.since)
	}
	return w.remaining
}

func (w *window) expired() bool { return w.active && w.left() <= 0 }

// pause stops the clock (an accepted continuation is open).
func (w *window) pause() {
	if w.active && w.running {
		w.remaining = w.left()
		w.running = false
	}
}

// resume restarts a paused clock.
func (w *window) resume() {
	if w.active && !w.running {
		w.since = w.clk.Now()
		w.running = true
	}
}

// end closes the window (data, a terminal result or a clean rotation).
func (w *window) end() { w.active = false }

// waitRetry sleeps the back-off before the next attempt. It returns false
// when the window ends before the delay passes.
func (w *window) waitRetry(ctx context.Context, b *backoff, f failure) (bool, error) {
	d := b.next(f.retryDelay)
	if d >= w.left() {
		return false, nil
	}
	if err := clock.Sleep(ctx, w.clk, d); err != nil {
		return false, err
	}
	return !w.expired(), nil
}

// grant is a relay grant that the SDK holds for one operation.
type grant struct {
	id string
	// receivedAt is when the SDK received the grant, on its clock.
	receivedAt time.Time
	// expiresIn is the remaining lifetime at receipt.
	expiresIn time.Duration
}

func (g *grant) expiry() time.Time { return g.receivedAt.Add(g.expiresIn) }

// opAuth holds the relay authorization of one operation. Each operation has
// its own grant; the SDK never uses it for another operation.
type opAuth struct {
	dp     *dataPath
	kind   ownsandv1.RelayOperationKind
	opID   string
	digest []byte

	g *grant
	// replaced is the ID of the last grant the SDK held, which the next
	// AuthorizeRelay names in replaces_grant_id.
	replaced string
	// lastStreamEnd is when the last stream of the current grant ended.
	lastStreamEnd time.Time
}

// streamKind selects the limit that a grant must outlive.
type streamKind int

const (
	serverStream streamKind = iota
	writeCall
)

// needsRenewal reports whether the grant must be renewed before a new
// stream of the given kind starts.
func (a *opAuth) needsRenewal(k streamKind) bool {
	if a.g == nil {
		return true
	}
	now := a.dp.c.clk.Now()
	limit := relayStreamLimit + grantMargin
	if k == writeCall {
		limit = writeCallLimit + grantMargin
	}
	if now.Add(limit).After(a.g.expiry()) {
		return true
	}
	last := a.g.receivedAt
	if a.lastStreamEnd.After(last) {
		last = a.lastStreamEnd
	}
	return now.Sub(last) > grantIdleRelease
}

// discard drops the grant after a failure; the next authorization replaces it.
func (a *opAuth) discard() {
	if a.g != nil {
		a.replaced = a.g.id
		a.g = nil
	}
}

func (a *opAuth) streamEnded() { a.lastStreamEnd = a.dp.c.clk.Now() }

// ensure authorizes or renews the grant before a new stream when needed.
// During recovery, rw is the open recovery window; otherwise ensure uses its
// own 60-second retry window.
func (a *opAuth) ensure(ctx context.Context, k streamKind, rw *window) error {
	if !a.needsRenewal(k) {
		return nil
	}
	if a.g != nil {
		a.replaced = a.g.id
		a.g = nil
	}
	w := rw
	if w == nil || !w.active {
		w = newWindow(a.dp.c.clk)
	}
	return a.authorize(ctx, w)
}

// authorize calls AuthorizeRelay with one authorization_id and retries it
// within w after a lost response, unavailable or resource_exhausted.
func (a *opAuth) authorize(ctx context.Context, w *window) error {
	c := a.dp.c
	req := &ownsandv1.AuthorizeRelayRequest{
		OrganizationId:  a.dp.org,
		SandboxId:       a.dp.sandboxID,
		OperationKind:   a.kind,
		OperationId:     a.opID,
		RequestDigest:   a.digest,
		AuthorizationId: datapath.NewUUIDv7At(c.clk.Now()).String(),
	}
	if a.replaced != "" {
		r := a.replaced
		req.ReplacesGrantId = &r
	}
	b := newBackoff()
	for {
		resp, err := c.dataPath.AuthorizeRelay(ctx, connect.NewRequest(req))
		if err == nil {
			a.g = &grant{id: resp.Msg.GetGrantId(), receivedAt: c.clk.Now(), expiresIn: resp.Msg.GetExpiresIn().AsDuration()}
			a.lastStreamEnd = time.Time{}
			c.log.Debug("relay grant received",
				slog.String("sandbox_id", a.dp.sandboxID),
				slog.String("operation_kind", a.kind.String()),
				slog.String("operation_id", a.opID),
				slog.String("grant_id", a.g.id),
				slog.Bool("renewal", req.ReplacesGrantId != nil))
			return nil
		}
		f := classify(ctx, err)
		c.log.Debug("AuthorizeRelay failed", slog.String("sandbox_id", a.dp.sandboxID), slog.String("code", f.code.String()), slog.Bool("transport", f.transport))
		if !f.retryable(false) {
			return err
		}
		w.start()
		ok, serr := w.waitRetry(ctx, b, f)
		if serr != nil {
			return serr
		}
		if !ok {
			return err
		}
	}
}

// DataPathError is a data path failure of an exec or file operation. It is
// never an exit code.
type DataPathError struct {
	// Op names the operation, for example "exec" or "read file".
	Op string
	// OutcomeUnknown is true for an exec whose outcome the SDK cannot know,
	// for example when AttachExec fails with not_found after started.
	OutcomeUnknown bool
	Err            error
}

func (e *DataPathError) Error() string {
	s := "ownsand: " + e.Op + " data path failed"
	if e.OutcomeUnknown {
		s += " (outcome unknown)"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *DataPathError) Unwrap() error { return e.Err }

// Code returns the Connect code of the underlying error.
func (e *DataPathError) Code() connect.Code { return CodeOf(e.Err) }
