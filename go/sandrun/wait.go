// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

// IsTerminalState reports whether a request or dev session state is
// terminal: ENDED, CANCELED, EXPIRED or FAILED.
func IsTerminalState(s sandrunv1.RequestState) bool {
	switch s {
	case sandrunv1.RequestState_REQUEST_STATE_ENDED, sandrunv1.RequestState_REQUEST_STATE_CANCELED,
		sandrunv1.RequestState_REQUEST_STATE_EXPIRED, sandrunv1.RequestState_REQUEST_STATE_FAILED:
		return true
	}
	return false
}

// WaitDevSessionParams describes a wait for a dev session.
type WaitDevSessionParams struct {
	OrganizationID string
	DevSessionID   string
	// Interval is the pause between GetDevSession calls. Zero means 2
	// seconds.
	Interval time.Duration
}

// WaitDevSession calls GetDevSession repeatedly until the dev session is
// RUNNING or terminal, and returns it. If ctx ends before the wait accepts
// a poll result, it returns the last accepted dev session (or nil) and
// ctx.Err(). The wait logs transient failures at debug level and retries
// them at the next poll. Other errors end the wait.
func (c *Client) WaitDevSession(ctx context.Context, p WaitDevSessionParams) (*sandrunv1.DevSession, error) {
	interval := p.Interval
	if interval <= 0 {
		interval = c.timing.waitInterval
	}
	var last *sandrunv1.DevSession
	for {
		res, err := c.devSessions.GetDevSession(ctx, connect.NewRequest(&sandrunv1.GetDevSessionRequest{
			OrganizationId: p.OrganizationID,
			DevSessionId:   p.DevSessionID,
		}))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return last, ctxErr
		}
		switch {
		case err == nil:
			last = res.Msg.GetDevSession()
			if s := last.GetState(); s == sandrunv1.RequestState_REQUEST_STATE_RUNNING || IsTerminalState(s) {
				return last, nil
			}
		case transient(ctx, err):
			c.logger.DebugContext(ctx, "sandrun wait dev session retries", "dev_session_id", p.DevSessionID, "code", codeText(err))
		default:
			return last, err
		}
		if err := sleep(ctx, interval); err != nil {
			return last, err
		}
	}
}
