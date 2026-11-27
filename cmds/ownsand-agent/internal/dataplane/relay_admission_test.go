// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestRelayStreamAdmissionRequiresActiveConnection(t *testing.T) {
	ch := newChannel(1)
	g := &grant{
		id: newID(t), sandboxID: newID(t), ch: ch, received: time.Now(),
		expiresIn: time.Hour, deadlineIn: time.Hour, streams: map[*rstream]bool{},
	}
	r := &relay{
		d:  &DataPlane{logger: logger(), timing: DefaultTiming()},
		ch: ch, limits: DefaultRelayLimits(), grants: map[string]*grant{g.id: g},
	}
	r.limits.MaxStreamsPerGrant = 1
	newConnection := func() *rconn {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &rconn{
			gen: ch.Generation(), ctx: ctx, cancel: cancel,
			out: make(chan *ownsandv1.DataFrame, 4), streams: map[uint64]*rstream{},
		}
	}
	open := func(c *rconn, id uint64, grantID string) {
		r.onFrame(c, datapath.MustNewFrame(id, g.sandboxID, &ownsandv1.RelayStreamOpen{
			GrantId: grantID, InitialWindowBytes: datapath.MinWindowSize,
		}))
	}
	requireIdle := func(t *testing.T, c *rconn) {
		t.Helper()
		if c.highest != 0 || c.credit != 0 || len(c.streams) != 0 || len(c.out) != 0 {
			t.Errorf("idle connection changed: highest=%d credit=%d streams=%d queued=%d",
				c.highest, c.credit, len(c.streams), len(c.out))
		}
	}

	current := newConnection()
	t.Run("retired", func(t *testing.T) {
		retired := newConnection()
		r.conn = retired
		r.connClosed(retired)
		r.conn = current

		// Keep the retired context live to exercise the connection identity check.
		open(retired, 1, g.id)
		requireIdle(t, retired)
		requireIdle(t, current)
		if len(g.streams) != 0 || r.conn != current {
			t.Errorf("retired open changed active relay state: grant streams=%d", len(g.streams))
		}
		r.connClosed(retired)
	})
	t.Run("canceled current", func(t *testing.T) {
		canceled := newConnection()
		r.conn = canceled
		canceled.cancel()

		open(canceled, 1, g.id)
		requireIdle(t, canceled)
		if len(g.streams) != 0 || r.conn != canceled {
			t.Errorf("canceled open changed active relay state: grant streams=%d", len(g.streams))
		}
		r.connClosed(canceled)
	})
	t.Run("live current", func(t *testing.T) {
		r.conn = current
		defer r.connClosed(current)
		next := func(id uint64) *ownsandv1.DataFrame {
			t.Helper()
			select {
			case f := <-current.out:
				if f.GetStreamId() != id || f.GetSandboxId() != g.sandboxID {
					t.Fatalf("response for stream %d: %v", id, f)
				}
				return f
			default:
				t.Fatalf("no response for stream %d", id)
				return nil
			}
		}

		open(current, 1, g.id)
		if f := next(1); f.GetRelayWindowUpdate().GetIncrementBytes() != r.limits.StreamReceiveWindow {
			t.Fatalf("initial window response: %v", f)
		}
		st := current.streams[1]
		if st == nil || !g.streams[st] || current.highest != 1 || current.credit != r.limits.StreamReceiveWindow {
			t.Fatal("live open did not register the stream and charge its receive credit")
		}

		open(current, 2, newID(t))
		if f := next(2); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
			t.Fatalf("invalid grant response: %v", f)
		}
		open(current, 3, g.id)
		if f := next(3); f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
			t.Fatalf("capacity response: %v", f)
		}
		if len(current.streams) != 1 || len(g.streams) != 1 || current.credit != r.limits.StreamReceiveWindow {
			t.Fatal("rejected opens changed stream counts or receive credit")
		}

		r.connClosed(current)
		if r.conn != nil || len(current.streams) != 0 || len(g.streams) != 0 || current.credit != 0 || st.ctx.Err() == nil {
			t.Fatal("connection cleanup did not remove the stream, cancel it, and return its receive credit")
		}
	})
}
