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
)

// credential produces the bearer value of a request.
type credential interface {
	bearer(ctx context.Context) (string, error)
}

type staticCredential string

func (s staticCredential) bearer(context.Context) (string, error) { return string(s), nil }

type tokenCredential TokenSource

func (t tokenCredential) bearer(ctx context.Context) (string, error) {
	tok, err := t(ctx)
	if err != nil {
		return "", fmt.Errorf("ownsand: token source: %w", err)
	}
	if tok == "" {
		return "", errors.New("ownsand: token source returned an empty token")
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
		out := connect.NewError(ce.Code(), errors.New(msg))
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
type activity struct {
	last atomic.Int64
}

func newActivity() *activity {
	a := &activity{}
	a.touch()
	return a
}

func (a *activity) touch() { a.last.Store(time.Now().UnixNano()) }

func (a *activity) idle() time.Duration {
	return time.Since(time.Unix(0, a.last.Load()))
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
