// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"
)

// errLocalCredTestSentinel is the error that the test token sources return.
var errLocalCredTestSentinel = errors.New("local credential store unavailable")

// localCredTokenSource is a token source whose behavior a test controls. The
// call numbers start at 1 and count every call, including those of requests
// that the SDK sends after the one under test.
type localCredTokenSource struct {
	mu    sync.Mutex
	calls int
	// failFrom and failTo select the calls (inclusive) that fail; zero means
	// no call fails.
	failFrom, failTo int
	// empty makes a failing call return an empty token instead of an error.
	empty bool
	// onFail, when set, replaces the error of a failing call.
	onFail func() error
}

func (ts *localCredTokenSource) fn(context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.calls++
	if ts.failFrom != 0 && ts.calls >= ts.failFrom && ts.calls <= ts.failTo {
		if ts.empty {
			return "", nil
		}
		if ts.onFail != nil {
			return "", ts.onFail()
		}
		return "", errLocalCredTestSentinel
	}
	return testAPIKey, nil
}

func (ts *localCredTokenSource) callCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls
}

// localCredClient builds an automatic-transport client that authenticates
// with ts.
func localCredClient(t *testing.T, cp *fakeCP, tr Transport, ts *localCredTokenSource) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: cp.srv.URL, TokenSource: ts.fn, OrganizationID: cp.orgID, Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	c.p2p.settingEngine = loopbackSettingEngine
	return c
}

func localCredExecOnce(s *Sandbox, ctx context.Context) (*ExecResult, error) {
	return s.Exec(ctx, ExecParams{Command: []string{"run"}})
}

func TestLocalCredentialFailureEndsOperationAndKeepsPeerRouting(t *testing.T) {
	cases := []struct {
		name string
		// failCall is the token source call that fails: 1 is GetIceServers,
		// 2 is OpenSession.
		failCall int
		empty    bool
	}{
		{name: "callback error on GetIceServers", failCall: 1},
		{name: "empty token on GetIceServers", failCall: 1, empty: true},
		{name: "callback error on OpenSession", failCall: 2},
		{name: "empty token on OpenSession", failCall: 2, empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = func(_ []string, e *fakeExec) { e.finish(exitedEvent(3)) }
			ts := &localCredTokenSource{failFrom: tc.failCall, failTo: tc.failCall, empty: tc.empty}
			s := cp.handle(t, localCredClient(t, cp, TransportAuto, ts))
			before := s.Transport()

			_, err := localCredExecOnce(s, context.Background())
			if err == nil {
				t.Fatal("exec succeeded although the token source failed")
			}
			if !tc.empty && !errors.Is(err, errLocalCredTestSentinel) {
				t.Fatalf("the error of the token source is not reachable: %v", err)
			}
			var lc *errLocalCredential
			if !errors.As(err, &lc) {
				t.Fatalf("exec error is not a local credential error: %v", err)
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls after a local credential failure", n)
			}
			if got := s.Transport(); got != before {
				t.Fatalf("transport changed from %v to %v", before, got)
			}
			s.dp.mu.Lock()
			relay := s.dp.relay
			s.dp.mu.Unlock()
			if relay {
				t.Fatal("the handle moved to the relay")
			}

			// The token source recovers: the next operation on the same
			// handle sets the session up again.
			iceBefore := cp.iceCallCount()
			res, err := localCredExecOnce(s, context.Background())
			if err != nil {
				t.Fatalf("exec after recovery: %v", err)
			}
			if res.Transport != p2pTransport || s.Transport() != p2pTransport {
				t.Fatalf("transport after recovery: result %v, handle %v", res.Transport, s.Transport())
			}
			if cp.iceCallCount() != iceBefore+1 || cp.openSessionCount() == 0 {
				t.Fatalf("no new peer setup after recovery: ice calls %d to %d, OpenSession calls %d", iceBefore, cp.iceCallCount(), cp.openSessionCount())
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls after recovery", n)
			}
		})
	}
}

func TestLocalCredentialFailureEndsFileOperationWithoutRelay(t *testing.T) {
	cp := newFakeCP(t)
	ts := &localCredTokenSource{failFrom: 1, failTo: 1}
	s := cp.handle(t, localCredClient(t, cp, TransportAuto, ts))

	_, err := s.ReadFileBytes(context.Background(), "/etc/hostname")
	if !errors.Is(err, errLocalCredTestSentinel) {
		t.Fatalf("read after a local credential failure: %v", err)
	}
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls after a local credential failure", n)
	}
	s.dp.mu.Lock()
	relay := s.dp.relay
	s.dp.mu.Unlock()
	if relay {
		t.Fatal("the handle moved to the relay")
	}
}

func TestLocalCredentialFailureWithForcedPeerToPeerReturnsTheError(t *testing.T) {
	cp := newFakeCP(t)
	ts := &localCredTokenSource{failFrom: 1, failTo: 1}
	s := cp.handle(t, localCredClient(t, cp, TransportPeerToPeer, ts))
	_, err := localCredExecOnce(s, context.Background())
	if !errors.Is(err, errLocalCredTestSentinel) {
		t.Fatalf("exec error: %v", err)
	}
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls", n)
	}
}

func TestControlPlaneSetupFailureStillFallsBackWithTokenSource(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = func(_ []string, e *fakeExec) { e.finish(exitedEvent(3)) }
	cp.mu.Lock()
	cp.openErr = connect.NewError(connect.CodeUnavailable, errors.New("control plane busy"))
	cp.mu.Unlock()
	ts := &localCredTokenSource{}
	s := cp.handle(t, localCredClient(t, cp, TransportAuto, ts))

	res, err := localCredExecOnce(s, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport != relayTransport || s.Transport() != relayTransport {
		t.Fatalf("transport %v, handle %v", res.Transport, s.Transport())
	}
	if len(cp.authRequests()) == 0 {
		t.Fatal("the relay was not used")
	}
}

func TestLocalCredentialFailureAfterCallerCancellationReturnsContextError(t *testing.T) {
	cp := newFakeCP(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The token source observes the caller's cancellation and reports it.
	ts := &localCredTokenSource{failFrom: 1, failTo: 1, onFail: func() error {
		cancel()
		return ctx.Err()
	}}
	s := cp.handle(t, localCredClient(t, cp, TransportAuto, ts))

	_, err := localCredExecOnce(s, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("exec error: %v", err)
	}
	var lc *errLocalCredential
	if errors.As(err, &lc) {
		t.Fatalf("a caller cancellation surfaced as a local credential failure: %v", err)
	}
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls", n)
	}
}
