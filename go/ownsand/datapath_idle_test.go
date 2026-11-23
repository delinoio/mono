// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

// idleEnd ends the installed session while no operation uses it, the way the
// peer, the network or the agent does.
type idleEnd struct {
	name string
	// failure reports whether the end is a channel failure (true) or a
	// routine end (false).
	failure bool
	end     func(t *testing.T, cp *fakeCP, sess *p2pSession)
}

var idleFailures = []idleEnd{
	{"channel closed without a session-end frame", true, func(t *testing.T, cp *fakeCP, _ *p2pSession) {
		cp.mu.Lock()
		pc := cp.answerers[0]
		cp.mu.Unlock()
		if err := pc.Close(); err != nil {
			t.Fatalf("close the agent peer connection: %v", err)
		}
	}},
	{"peer connection failure", true, func(_ *testing.T, _ *fakeCP, sess *p2pSession) {
		sess.end(&errP2PFailed{cause: errors.New("peer connection failed")})
	}},
	{"invalid byte stream", true, func(_ *testing.T, _ *fakeCP, sess *p2pSession) {
		// A length varint that never ends is a malformed stream.
		sess.onMessage(webrtc.DataChannelMessage{IsString: false, Data: bytes.Repeat([]byte{0xff}, 32)})
	}},
}

var idleRoutineEnds = []idleEnd{
	{"valid session-end frame", false, func(_ *testing.T, _ *fakeCP, sess *p2pSession) {
		sess.dispatch(datapath.MustNewFrame(1, "", &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED}))
	}},
	{"retirement", false, func(_ *testing.T, _ *fakeCP, sess *p2pSession) {
		sess.end(&errSessionRetired{})
	}},
}

// openIdleSession runs one operation, which opens the peer-to-peer session,
// and returns that session, idle.
func openIdleSession(t *testing.T, cp *fakeCP, s *Sandbox) *p2pSession {
	t.Helper()
	if _, err := execOnce(t, s); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	sess := s.installedSession()
	if sess == nil {
		t.Fatal("no session is installed after the first exec")
	}
	if cp.iceCallCount() != 1 || cp.openSessionCount() != 1 {
		t.Fatalf("setup calls after the first exec: ice %d, open %d, want 1 and 1", cp.iceCallCount(), cp.openSessionCount())
	}
	return sess
}

func endIdle(t *testing.T, cp *fakeCP, sess *p2pSession, e idleEnd) {
	t.Helper()
	e.end(t, cp, sess)
	select {
	case <-sess.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
}

// TestIdleSessionFailureMovesAutomaticHandleToRelay checks that a session
// that fails while no operation uses it moves an automatic handle to the
// relay: the next and every later operation use the relay without another
// GetIceServers or OpenSession call.
func TestIdleSessionFailureMovesAutomaticHandleToRelay(t *testing.T) {
	for _, tc := range idleFailures {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportAuto))
			sess := openIdleSession(t, cp, s)
			endIdle(t, cp, sess, tc)

			for i := range 3 {
				res, err := execOnce(t, s)
				if err != nil || res == nil {
					t.Fatalf("exec %d after the idle failure: %v", i+1, err)
				}
				if got := s.Transport(); got != relayTransport {
					t.Fatalf("transport %v after exec %d, want relay", got, i+1)
				}
			}
			if cp.iceCallCount() != 1 || cp.openSessionCount() != 1 {
				t.Fatalf("setup calls after the idle failure: ice %d, open %d, want 1 and 1", cp.iceCallCount(), cp.openSessionCount())
			}
			if n := cp.relayCount("Exec"); n != 3 {
				t.Fatalf("%d relay Exec calls, want 3", n)
			}
			if !s.usesRelay() {
				t.Fatal("the handle did not move to the relay")
			}
			if s.installedSession() != nil {
				t.Fatal("the failed session is still installed")
			}
		})
	}
}

// TestIdleRoutineSessionEndKeepsDirectSetupAvailable checks that a valid
// session-end frame or a retirement of an idle session is not a failure: the
// next operation opens a new session on the same transport and uses no relay.
func TestIdleRoutineSessionEndKeepsDirectSetupAvailable(t *testing.T) {
	for _, tc := range idleRoutineEnds {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportAuto))
			sess := openIdleSession(t, cp, s)
			endIdle(t, cp, sess, tc)

			if _, err := execOnce(t, s); err != nil {
				t.Fatalf("exec after the idle end: %v", err)
			}
			if cp.iceCallCount() != 2 || cp.openSessionCount() != 2 {
				t.Fatalf("setup calls: ice %d, open %d, want 2 and 2 (a new session)", cp.iceCallCount(), cp.openSessionCount())
			}
			if s.usesRelay() || s.Transport() != p2pTransport {
				t.Fatalf("the handle left peer-to-peer (transport %v)", s.Transport())
			}
			if n := len(cp.authRequests()) + cp.relayCount("Exec"); n != 0 {
				t.Fatalf("%d relay requests after a routine session end, want 0", n)
			}
			if got := s.installedSession(); got == nil || got == sess {
				t.Fatal("no new session is installed")
			}
		})
	}
}

// TestIdleSessionFailureOnForcedPeerToPeerReopens checks that a forced
// peer-to-peer handle never moves to the relay: after an idle failure the
// next operation opens a new session.
func TestIdleSessionFailureOnForcedPeerToPeerReopens(t *testing.T) {
	for _, tc := range idleFailures {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			sess := openIdleSession(t, cp, s)
			endIdle(t, cp, sess, tc)

			if _, err := execOnce(t, s); err != nil {
				t.Fatalf("exec after the idle failure: %v", err)
			}
			if cp.iceCallCount() != 2 || cp.openSessionCount() != 2 {
				t.Fatalf("setup calls: ice %d, open %d, want 2 and 2", cp.iceCallCount(), cp.openSessionCount())
			}
			if s.usesRelay() || s.Transport() != p2pTransport {
				t.Fatalf("the forced handle left peer-to-peer (transport %v)", s.Transport())
			}
			if n := len(cp.authRequests()) + cp.relayCount("Exec"); n != 0 {
				t.Fatalf("%d relay requests on a forced peer-to-peer handle, want 0", n)
			}
		})
	}
}

// TestConcurrentOperationsAfterIdleFailureUseRelay checks that operations that
// start together after an idle failure all use the relay and none of them
// sets up a session.
func TestConcurrentOperationsAfterIdleFailureUseRelay(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportAuto))
	sess := openIdleSession(t, cp, s)
	endIdle(t, cp, sess, idleFailures[1])

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = execOnce(t, s)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent exec %d: %v", i, err)
		}
	}
	if cp.iceCallCount() != 1 || cp.openSessionCount() != 1 {
		t.Fatalf("setup calls: ice %d, open %d, want 1 and 1", cp.iceCallCount(), cp.openSessionCount())
	}
	if got := cp.relayCount("Exec"); got != n {
		t.Fatalf("%d relay Exec calls, want %d", got, n)
	}
}

// TestOldSessionFailureDoesNotChangeTheRouteOfItsReplacement checks that the
// terminal cause of a session that is no longer installed does not move the
// handle to the relay or forget the session that replaced it.
func TestOldSessionFailureDoesNotChangeTheRouteOfItsReplacement(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportAuto))
	old := openIdleSession(t, cp, s)
	endIdle(t, cp, old, idleRoutineEnds[1])
	if _, err := execOnce(t, s); err != nil {
		t.Fatalf("exec on the replacement: %v", err)
	}
	replacement := s.installedSession()
	if replacement == nil || replacement == old {
		t.Fatal("no replacement session is installed")
	}

	// An operation of the old session reports a failure late.
	s.dp.handleSessionError(old, &errP2PFailed{cause: errors.New("late failure of the old session")})

	if s.usesRelay() || s.Transport() != p2pTransport {
		t.Fatalf("a late failure of the old session moved the handle (transport %v)", s.Transport())
	}
	if s.installedSession() != replacement {
		t.Fatal("a late failure of the old session removed its replacement")
	}
	if _, err := execOnce(t, s); err != nil {
		t.Fatalf("exec after the late failure: %v", err)
	}
	if cp.iceCallCount() != 2 || cp.openSessionCount() != 2 {
		t.Fatalf("setup calls: ice %d, open %d, want 2 and 2", cp.iceCallCount(), cp.openSessionCount())
	}
}

// TestCloseAfterIdleFailureStillBlocksSetup checks that a closed handle
// rejects every operation with errHandleClosed after an idle failure, on the
// automatic and the forced transport, and calls nothing.
func TestCloseAfterIdleFailureStillBlocksSetup(t *testing.T) {
	for _, tr := range []Transport{TransportAuto, TransportPeerToPeer} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, tr))
			sess := openIdleSession(t, cp, s)
			endIdle(t, cp, sess, idleFailures[1])
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}}); !errors.Is(err, errHandleClosed) {
				t.Fatalf("exec after the close: %v, want errHandleClosed", err)
			}
			if cp.iceCallCount() != 1 || cp.openSessionCount() != 1 {
				t.Fatalf("setup calls: ice %d, open %d, want 1 and 1", cp.iceCallCount(), cp.openSessionCount())
			}
			if n := len(cp.authRequests()) + cp.relayCount("Exec"); n != 0 {
				t.Fatalf("%d relay requests after the close, want 0", n)
			}
		})
	}
}
