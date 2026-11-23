// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http/httpguts"
)

// credential produces the bearer value of a request.
type credential interface {
	bearer(ctx context.Context) (string, error)
}

type staticCredential string

func (s staticCredential) bearer(context.Context) (string, error) {
	if err := checkBearer(string(s)); err != nil {
		return "", &errLocalCredential{cause: err}
	}
	return string(s), nil
}

// errInvalidCredential reports a credential that is not a valid HTTP header
// field value, for example one that contains a newline or another control
// character. Go's HTTP transport rejects such a header before it sends the
// request, and the Connect client reports that rejection as an unavailable
// status, which the SDK would take for a transport interruption: it would
// retry, start recovery or move the handle to the relay although every
// attempt fails the same way. The SDK therefore checks the value first and
// ends the operation with this error. The message never contains the
// credential.
var errInvalidCredential = errors.New("ownsand: the credential is not a valid HTTP Authorization header value: remove control characters such as newlines from the API key or the token")

// checkBearer returns errInvalidCredential when "Bearer "+tok cannot be sent
// as an Authorization header value. An empty token is the concern of the
// callers.
func checkBearer(tok string) error {
	if !httpguts.ValidHeaderFieldValue("Bearer " + tok) {
		return errInvalidCredential
	}
	return nil
}

type tokenCredential TokenSource

// errLocalCredential marks a failure to produce the bearer value on this
// machine: the token source returned an error, an empty token or a value that
// is not a valid Authorization header value (errInvalidCredential), or the
// fixed API key is not a valid header value. No request
// left the process, so the failure says nothing about the control plane or
// the data path. The data path router therefore ends the operation with it
// and neither falls back to the relay nor moves the handle off peer-to-peer;
// a later operation asks the token source again.
type errLocalCredential struct {
	cause error
}

func (e *errLocalCredential) Error() string { return e.cause.Error() }

// Unwrap keeps errors.Is and errors.As working on the error that the token
// source returned.
func (e *errLocalCredential) Unwrap() error { return e.cause }

func (t tokenCredential) bearer(ctx context.Context) (string, error) {
	tok, err := t(ctx)
	if err != nil {
		return "", &errLocalCredential{cause: fmt.Errorf("ownsand: token source: %w", err)}
	}
	if tok == "" {
		return "", &errLocalCredential{cause: errors.New("ownsand: token source returned an empty token")}
	}
	if err := checkBearer(tok); err != nil {
		return "", &errLocalCredential{cause: err}
	}
	return tok, nil
}

// authInterceptor sets the Authorization header. The Connect clients are
// bound to the configured origin and the HTTP client follows no redirects,
// so the credential reaches only that origin.
type authInterceptor struct {
	client *Client
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tok, err := a.client.credential.bearer(ctx)
		if err != nil {
			return nil, err
		}
		req.Header().Set("Authorization", "Bearer "+tok)
		start := time.Now()
		res, err := next(ctx, req)
		a.client.logRPC(ctx, req.Spec().Procedure, start, err)
		return res, a.client.redact(err, tok)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		tok, err := a.client.credential.bearer(ctx)
		if err != nil {
			return &failedConn{StreamingClientConn: conn, err: err}
		}
		conn.RequestHeader().Set("Authorization", "Bearer "+tok)
		return &redactingConn{StreamingClientConn: conn, client: a.client, token: tok, start: time.Now(), ctx: ctx}
	}
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// failedConn reports a credential failure on the first send.
type failedConn struct {
	connect.StreamingClientConn
	err error
}

func (f *failedConn) Send(any) error             { return f.err }
func (f *failedConn) Receive(any) error          { return f.err }
func (f *failedConn) CloseRequest() error        { return nil }
func (f *failedConn) CloseResponse() error       { return nil }
func (f *failedConn) RequestHeader() http.Header { return http.Header{} }

type redactingConn struct {
	connect.StreamingClientConn
	client *Client
	token  string
	start  time.Time
	ctx    context.Context
	logged atomic.Bool
}

func (r *redactingConn) Send(m any) error {
	return r.client.redact(r.StreamingClientConn.Send(m), r.token)
}

func (r *redactingConn) Receive(m any) error {
	err := r.StreamingClientConn.Receive(m)
	if err != nil && r.logged.CompareAndSwap(false, true) {
		logErr := err
		if errors.Is(err, io.EOF) {
			logErr = nil
		}
		r.client.logRPC(r.ctx, r.Spec().Procedure, r.start, logErr)
	}
	return r.client.redact(err, r.token)
}

// redact removes the credential from an error message while keeping the
// Connect code, details and wire flag of a Connect error.
func (c *Client) redact(err error, secret string) error {
	if err == nil || secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg := strings.ReplaceAll(ce.Message(), secret, "[REDACTED]")
		// connect.NewError drops the wire flag, and classify treats a wire
		// error as an explicit server status, so rebuild a wire error as a
		// wire error and a local error as a local error.
		var out *connect.Error
		if connect.IsWireError(ce) {
			out = connect.NewWireError(ce.Code(), errors.New(msg))
		} else {
			out = connect.NewError(ce.Code(), errors.New(msg))
		}
		for k, v := range ce.Meta() {
			out.Meta()[k] = v
		}
		for _, d := range ce.Details() {
			out.AddDetail(d)
		}
		return out
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED]"))
}

func (c *Client) logRPC(ctx context.Context, procedure string, start time.Time, err error) {
	attrs := []any{"rpc", procedure, "duration", time.Since(start)}
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String())
		c.logger.DebugContext(ctx, "ownsand rpc failed", attrs...)
		return
	}
	c.logger.DebugContext(ctx, "ownsand rpc", attrs...)
}

// activity records the time of the last received response byte of one
// request, so that the relay can detect a stream that receives nothing for
// 90 seconds, also while one large frame is still in transit.
//
// It stores the elapsed time since base, not a Unix timestamp. base keeps the
// monotonic clock reading of time.Now, so time.Since(base) ignores a step of
// the wall clock (for example an NTP correction or a manual change): the step
// alone neither expires a live stream nor extends the limit of a stalled one.
// Do not rebuild base from a Unix time or call Round(0), Truncate(0) or
// UTC/Local conversions that strip the monotonic reading.
type activity struct {
	base time.Time
	// elapsed returns the monotonic time since base. Tests replace it with a
	// controlled clock; production code leaves it nil and uses time.Since.
	elapsed func() time.Duration
	last    atomic.Int64 // nanoseconds since base of the last received byte
}

func newActivity() *activity {
	a := &activity{base: time.Now()}
	a.touch()
	return a
}

func (a *activity) now() time.Duration {
	if a.elapsed != nil {
		return a.elapsed()
	}
	return time.Since(a.base)
}

func (a *activity) touch() { a.last.Store(int64(a.now())) }

func (a *activity) idle() time.Duration {
	return max(a.now()-time.Duration(a.last.Load()), 0)
}

type activityKey struct{}

func withActivity(ctx context.Context, a *activity) context.Context {
	return context.WithValue(ctx, activityKey{}, a)
}

// activityTransport wraps response bodies of requests whose context carries
// an activity tracker.
type activityTransport struct {
	next http.RoundTripper
}

func (t *activityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.next.RoundTrip(req)
	if err != nil {
		return res, err
	}
	if a, ok := req.Context().Value(activityKey{}).(*activity); ok && res.Body != nil {
		a.touch()
		res.Body = &activityBody{ReadCloser: res.Body, a: a}
	}
	return res, nil
}

type activityBody struct {
	io.ReadCloser
	a *activity
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.a.touch()
	}
	return n, err
}
