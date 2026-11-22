// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// IsTerminalState reports whether a request or dev session state is
// terminal: ENDED, CANCELED, EXPIRED or FAILED.
func IsTerminalState(s tidev1.RequestState) bool {
	switch s {
	case tidev1.RequestState_REQUEST_STATE_ENDED, tidev1.RequestState_REQUEST_STATE_CANCELED,
		tidev1.RequestState_REQUEST_STATE_EXPIRED, tidev1.RequestState_REQUEST_STATE_FAILED:
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
// RUNNING or terminal, and returns it. When ctx ends first (for example at
// the caller's deadline), it returns the last dev session that it read
// (nil when it read none) together with ctx.Err(). Transient failures are
// retried at the next poll and logged at debug level; other errors end the
// wait.
func (c *Client) WaitDevSession(ctx context.Context, p WaitDevSessionParams) (*tidev1.DevSession, error) {
	interval := p.Interval
	if interval <= 0 {
		interval = c.timing.waitInterval
	}
	var last *tidev1.DevSession
	for {
		res, err := c.devSessions.GetDevSession(ctx, connect.NewRequest(&tidev1.GetDevSessionRequest{
			OrganizationId: p.OrganizationID,
			DevSessionId:   p.DevSessionID,
		}))
		switch {
		case err == nil:
			last = res.Msg.GetDevSession()
			if s := last.GetState(); s == tidev1.RequestState_REQUEST_STATE_RUNNING || IsTerminalState(s) {
				return last, nil
			}
		case ctx.Err() != nil:
			return last, ctx.Err()
		case transient(ctx, err):
			c.logger.DebugContext(ctx, "tide wait dev session retries", "dev_session_id", p.DevSessionID, "code", codeText(err))
		default:
			return last, err
		}
		if err := sleep(ctx, interval); err != nil {
			return last, err
		}
	}
}
