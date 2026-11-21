// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
)

// LogInterceptor writes one `rpc` record for every RPC: the procedure, the
// Connect code, the caller's Logto `sub` and the duration. It never logs
// requests or responses, which can hold invitation tokens and email
// addresses. It must run before the authentication interceptor so that it
// also logs rejected calls.
type LogInterceptor struct {
	logger *slog.Logger
}

// NewLogInterceptor returns the RPC log interceptor.
func NewLogInterceptor(logger *slog.Logger) *LogInterceptor {
	return &LogInterceptor{logger: logger}
}

// callerSlot lets the authentication interceptor, which runs inside the log
// interceptor, report the caller back to it.
type callerSlot struct{ userID string }

type callerSlotKey struct{}

// CallerObserver is a connect.Interceptor that records the authenticated
// caller for the RPC log record. Install it after the authentication
// interceptor.
type CallerObserver struct{}

func (CallerObserver) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		observeCaller(ctx)
		return next(ctx, req)
	}
}

func (CallerObserver) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (CallerObserver) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		observeCaller(ctx)
		return next(ctx, conn)
	}
}

func observeCaller(ctx context.Context) {
	slot, _ := ctx.Value(callerSlotKey{}).(*callerSlot)
	if caller, ok := auth.CallerFrom(ctx); ok && slot != nil {
		slot.userID = caller.UserID()
	}
}

func (l *LogInterceptor) log(ctx context.Context, procedure string, slot *callerSlot, start time.Time, err error) {
	l.logger.LogAttrs(ctx, slog.LevelInfo, "rpc",
		slog.String("procedure", procedure),
		slog.String("code", codeString(err)),
		slog.String("user_id", slot.userID),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
}

// codeString is the Connect code that the log records for err.
func codeString(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled.String()
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded.String()
	}
	return connect.CodeOf(err).String()
}

func (l *LogInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		slot := &callerSlot{}
		resp, err := next(context.WithValue(ctx, callerSlotKey{}, slot), req)
		l.log(ctx, req.Spec().Procedure, slot, start, err)
		return resp, err
	}
}

func (l *LogInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l *LogInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		slot := &callerSlot{}
		err := next(context.WithValue(ctx, callerSlotKey{}, slot), conn)
		l.log(ctx, conn.Spec().Procedure, slot, start, err)
		return err
	}
}
