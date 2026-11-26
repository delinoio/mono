// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"errors"
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
		var networkError interface{ Timeout() bool }
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
			return "", errorf("token source: %w", err)
		}
		return "", localError(connect.CodeUnauthenticated, &CredentialError{err: err})
	}
	if tok == "" {
		return "", localError(connect.CodeUnauthenticated, &CredentialError{})
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
		return res, redact(err, tok)
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

// failedConn reports a credential failure on the first send or receive.
type failedConn struct {
	connect.StreamingClientConn
	err error
}

func (f *failedConn) Send(any) error    { return f.err }
func (f *failedConn) Receive(any) error { return f.err }

// Connect treats EOF on send and receive as a stream boundary. Return the
// error on close too so a token source error that wraps EOF reaches callers.
func (f *failedConn) CloseRequest() error        { return f.err }
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
	return redact(r.StreamingClientConn.Send(m), r.token)
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
	return redact(err, r.token)
}

// redact removes the credential from an error message while keeping the
// Connect code, metadata, details and wire flag of a Connect error.
func redact(err error, secret string) error {
	if err == nil || secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg := strings.ReplaceAll(ce.Message(), secret, "[REDACTED]")
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

// logRPC logs the procedure, duration and Connect code of one RPC at debug
// level. It never logs the request or response message.
func (c *Client) logRPC(ctx context.Context, procedure string, start time.Time, err error) {
	attrs := []any{"rpc", procedure, "duration", time.Since(start)}
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String())
		c.logger.DebugContext(ctx, "sandrun rpc failed", attrs...)
		return
	}
	c.logger.DebugContext(ctx, "sandrun rpc", attrs...)
}
