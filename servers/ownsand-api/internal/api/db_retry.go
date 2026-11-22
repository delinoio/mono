// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// DefaultCreateFailTimeout bounds the retry of the end-of-record update of
// an unsent or rejected create.
const DefaultCreateFailTimeout = 5 * time.Second

func (s *Service) createFailTimeout() time.Duration {
	if s.Timing.CreateFailTimeout > 0 {
		return s.Timing.CreateFailTimeout
	}
	return DefaultCreateFailTimeout
}

// retryTransientDB runs fn until it succeeds, fails with an error that is
// not transient (see isTransientDBError), or ctx ends. After a transient
// error it logs event at warn level with attrs, the attempt number, the wait
// and the error, waits with a doubling backoff that starts at
// Timing.ExportApplyRetryBackoff and stops at exportApplyRetryBackoffMax,
// and runs fn again. It returns the number of attempts and the error of the
// last attempt. fn must be safe to repeat: a retry follows an attempt whose
// commit outcome may be unknown, so fn applies guarded updates only.
func (s *Service) retryTransientDB(ctx context.Context, event string, attrs []any, fn func(context.Context) error) (int, error) {
	backoff := s.exportApplyRetryBackoff()
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || !isTransientDBError(err) || ctx.Err() != nil {
			return attempt, err
		}
		s.Logger.WarnContext(ctx, event, append(append([]any(nil), attrs...), slog.Int("attempt", attempt), slog.Duration("backoff", backoff), slog.Any("error", err))...)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
		if backoff *= 2; backoff > exportApplyRetryBackoffMax {
			backoff = exportApplyRetryBackoffMax
		}
	}
}

// errCreateClaimNotReleased is the cause that closes a channel whose unsent
// or rejected create could not be ended in the database.
var errCreateClaimNotReleased = errors.New("the claim of an unsent create could not be released")

// failUnsentCreate runs fn, the guarded update that ends the record of a
// create whose command was not queued or whose result the agent rejected (the
// reservation of a sandbox, the CREATING snapshot). It runs under a bounded
// context that keeps the values of ctx but not its cancellation, because a
// canceled request is a common cause of the send failure, and retries after a
// transient database error with retryTransientDB until
// Timing.CreateFailTimeout ends. The budget stays far below the heartbeat
// timeout because the channel read loop also runs this for an agent-rejected
// create.
//
// A permanent error or the end of the budget leaves the record CREATING with
// the reservation held, and no later event of the live channel would release
// it. The helper then closes conn, but only while it is still the current
// channel of its device, so the state report of the replacement channel
// reconciles the record (a sandbox the agent does not report becomes
// LOST_ON_DEVICE; a snapshot it does not list is failed) and a replacement
// channel that is already current is never closed. It returns the number of
// attempts, whether it closed conn, and the final error; the caller logs the
// outcome and keeps its own response.
func (s *Service) failUnsentCreate(ctx context.Context, conn *hub.Conn, event string, attrs []any, fn func(context.Context) error) (attempts int, channelClosed bool, err error) {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.createFailTimeout())
	defer cancel()
	attempts, err = s.retryTransientDB(dbCtx, event, attrs, fn)
	if err == nil {
		return attempts, false, nil
	}
	if s.Hub.Get(conn.DeviceID) == conn {
		conn.Close(errCreateClaimNotReleased)
		channelClosed = true
	}
	return attempts, channelClosed, err
}
