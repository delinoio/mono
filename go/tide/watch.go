// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// QueueEventKind is the kind of a WatchQueue event. Keepalive messages never
// reach the caller.
type QueueEventKind int

const (
	// QueueEventRequest carries a request of the snapshot or a changed
	// request.
	QueueEventRequest QueueEventKind = iota + 1
	// QueueEventRunner carries a runner of the snapshot or a changed
	// runner.
	QueueEventRunner
	// QueueEventSnapshotComplete follows the last request and runner of the
	// snapshot that each stream sends first.
	QueueEventSnapshotComplete
	// QueueEventGap reports that the stream ended and WatchQueue
	// reconnects. Changes between the two streams are not reported one by
	// one: the caller reloads its state from the snapshot of the new
	// stream, which follows this event.
	QueueEventGap
)

// String returns the stable identifier of k.
func (k QueueEventKind) String() string {
	switch k {
	case QueueEventRequest:
		return "request"
	case QueueEventRunner:
		return "runner"
	case QueueEventSnapshotComplete:
		return "snapshot_complete"
	case QueueEventGap:
		return "gap"
	}
	return fmt.Sprintf("QueueEventKind(%d)", int(k))
}

// QueueEvent is one event of WatchQueue.
type QueueEvent struct {
	Kind    QueueEventKind
	Request *tidev1.Request
	Runner  *tidev1.Runner
	// Err is the reason of the reconnect of a QueueEventGap event: nil
	// when the server ended the stream normally (it ends every stream
	// before 14 minutes).
	Err error
	// RetryIn is the delay before the reconnect of a QueueEventGap event.
	RetryIn time.Duration
}

// WatchQueue streams the request and runner changes of an organization to
// handle until ctx ends, handle returns an error, or the server rejects the
// watch with a status that a reconnect cannot fix (for example
// unauthenticated, permission_denied, not_found or invalid_argument). After
// each stream end it reconnects with exponential backoff from 1 second to
// 30 seconds and reports a QueueEventGap first; the backoff starts again
// from 1 second after a stream that delivered its snapshot. It returns
// ctx.Err() when ctx ends and the handler's error unchanged.
func (c *Client) WatchQueue(ctx context.Context, organizationID string, handle func(QueueEvent) error) error {
	bo := backoff{min: c.timing.backoffMin, max: c.timing.backoffMax}
	for attempt := 0; ; attempt++ {
		healthy, err := c.watchOnce(ctx, organizationID, handle)
		var he *handlerError
		if errors.As(err, &he) {
			return he.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !transient(ctx, err) {
			return err
		}
		if healthy {
			bo.reset()
		}
		delay := bo.next()
		c.logger.DebugContext(ctx, "tide watch queue reconnects", "attempt", attempt+1, "delay", delay, "code", codeText(err))
		if herr := handle(QueueEvent{Kind: QueueEventGap, Err: err, RetryIn: delay}); herr != nil {
			return herr
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// handlerError marks an error of the caller's handler.
type handlerError struct{ err error }

func (e *handlerError) Error() string { return e.err.Error() }
func (e *handlerError) Unwrap() error { return e.err }

// watchOnce runs one WatchQueue stream. healthy reports that the stream
// delivered its snapshot.
func (c *Client) watchOnce(ctx context.Context, organizationID string, handle func(QueueEvent) error) (healthy bool, _ error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.work.WatchQueue(ctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: organizationID}))
	if err != nil {
		return false, err
	}
	defer stream.Close()
	for stream.Receive() {
		msg := stream.Msg()
		var ev QueueEvent
		switch {
		case msg.GetRequest() != nil:
			ev = QueueEvent{Kind: QueueEventRequest, Request: msg.GetRequest()}
		case msg.GetRunner() != nil:
			ev = QueueEvent{Kind: QueueEventRunner, Runner: msg.GetRunner()}
		case msg.GetSnapshotComplete() != nil:
			healthy = true
			ev = QueueEvent{Kind: QueueEventSnapshotComplete}
		default:
			continue
		}
		if err := handle(ev); err != nil {
			return healthy, &handlerError{err: err}
		}
	}
	return healthy, stream.Err()
}

// codeText returns the Connect code of err for logs, or "ok".
func codeText(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}
