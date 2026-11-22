// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// recorder collects exec events.
type recorder struct {
	mu     sync.Mutex
	stdout bytes.Buffer
	stderr bytes.Buffer
	kinds  []ExecEventKind
	events []ExecEvent
}

func (r *recorder) on(ev ExecEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, ev.Kind)
	r.events = append(r.events, ev)
	switch ev.Kind {
	case ExecEventStdout:
		if ev.Offset != int64(r.stdout.Len()) {
			return fmt.Errorf("stdout chunk at %d, have %d bytes", ev.Offset, r.stdout.Len())
		}
		r.stdout.Write(ev.Data)
	case ExecEventStderr:
		if ev.Offset != int64(r.stderr.Len()) {
			return fmt.Errorf("stderr chunk at %d, have %d bytes", ev.Offset, r.stderr.Len())
		}
		r.stderr.Write(ev.Data)
	case ExecEventOutputTruncated:
		// Lost bytes: pad, so offsets keep matching.
		r.stdout.Write(make([]byte, ev.MissingStdout.To-ev.MissingStdout.From))
		r.stderr.Write(make([]byte, ev.MissingStderr.To-ev.MissingStderr.From))
	}
	return nil
}

func (r *recorder) count(k ExecEventKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.kinds {
		if x == k {
			n++
		}
	}
	return n
}

// slowRun writes n numbered lines, waiting for a gate before each line
// after the first, so that a test can interrupt the stream between lines.
func lineRun(n int, gap time.Duration) func([]string, *fakeExec) {
	return func(_ []string, e *fakeExec) {
		for i := range n {
			e.write(0, []byte(fmt.Sprintf("line %d\n", i)))
			time.Sleep(gap)
		}
		e.write(1, []byte("done\n"))
		e.finish(exitedEvent(3))
	}
}

func expectedLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func runExec(t *testing.T, s *Sandbox) (*ExecResult, *recorder) {
	t.Helper()
	r := &recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.Exec(ctx, ExecParams{Command: []string{"run"}, OnEvent: r.on})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return res, r
}

func checkLines(t *testing.T, res *ExecResult, r *recorder, n int) {
	t.Helper()
	if res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("result %+v", res)
	}
	if got := r.stdout.String(); got != expectedLines(n) {
		t.Fatalf("stdout %q", got)
	}
	if r.stderr.String() != "done\n" {
		t.Fatalf("stderr %q", r.stderr.String())
	}
	if r.count(ExecEventStarted) != 1 {
		t.Fatalf("%d started events", r.count(ExecEventStarted))
	}
}

func TestExecPeerToPeer(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(5, 0)
	s := cp.handle(t, cp.client(t, TransportAuto))
	res, r := runExec(t, s)
	checkLines(t, res, r, 5)
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER || s.Transport() != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER {
		t.Fatalf("transport %v", res.Transport)
	}
	if len(cp.authRequests()) != 0 {
		t.Fatal("AuthorizeRelay called on the peer-to-peer path")
	}
	if r.kinds[0] != ExecEventStarted {
		t.Fatalf("first event %v", r.kinds[0])
	}
}

func TestExecRelayOnlyWithoutOpenSession(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(5, 0)
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 5)
	if cp.openSessionCount() != 0 {
		t.Fatal("OpenSession called with the relay forced")
	}
	auths := cp.authRequests()
	if len(auths) != 1 || auths[0].GetOperationKind() != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || len(auths[0].GetRequestDigest()) != 32 {
		t.Fatalf("authorizations %v", auths)
	}
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("transport %v", res.Transport)
	}
}

func TestExecFallsBackToRelayWhenICEFails(t *testing.T) {
	cp := newFakeCP(t)
	cp.iceFail = true
	cp.agent.run = lineRun(3, 0)
	c := cp.client(t, TransportAuto)
	c.timing.p2pOpenTimeout = 500 * time.Millisecond
	s := cp.handle(t, c)
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("transport %v", res.Transport)
	}
	// The handle stays on the relay: a second exec opens no new session.
	before := cp.openSessionCount()
	runExec(t, s)
	if cp.openSessionCount() != before {
		t.Fatal("a second session opened after the fallback")
	}
}

func TestExecContinuesOnNewSessionAfterSessionEnd(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(6, 20*time.Millisecond)
	cp.p2pScript = func(n int) p2pAction {
		if n == 1 {
			return p2pAction{endAfter: 2, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE}
		}
		return p2pAction{}
	}
	s := cp.handle(t, cp.client(t, TransportAuto))
	res, r := runExec(t, s)
	checkLines(t, res, r, 6)
	if cp.openSessionCount() != 2 {
		t.Fatalf("%d sessions, want 2", cp.openSessionCount())
	}
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER {
		t.Fatalf("transport %v", res.Transport)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("command ran %d times", cp.agent.totalRuns())
	}
}

// quietRotations is more session ends than the failure cap of 5 allows.
const quietRotations = 8

// TestExecQuietSessionExpiriesDoNotHitFailureCap simulates a silent exec that
// outlives more session expiries than the failure cap: every session ends
// after the agent served the continuation and before any output arrives.
func TestExecQuietSessionExpiriesDoNotHitFailureCap(t *testing.T) {
	cp := newFakeCP(t)
	gate := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		<-gate
		e.write(0, []byte("line 0\n"))
		e.write(1, []byte("done\n"))
		e.finish(exitedEvent(3))
	}
	cp.p2pScript = func(n int) p2pAction {
		if n <= quietRotations {
			return p2pAction{endAfterStarted: true, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED}
		}
		if n == quietRotations+1 {
			close(gate)
		}
		return p2pAction{}
	}
	s := cp.handle(t, cp.client(t, TransportAuto))
	res, r := runExec(t, s)
	checkLines(t, res, r, 1)
	if cp.openSessionCount() != quietRotations+1 {
		t.Fatalf("%d sessions, want %d", cp.openSessionCount(), quietRotations+1)
	}
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER {
		t.Fatalf("transport %v", res.Transport)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("command ran %d times", cp.agent.totalRuns())
	}
}

// TestExecRepeatedSessionFailuresHitFailureCap checks that real failures
// still end the exec: session ends before the agent served the request, and
// a data channel that closes without a session-end frame on a forced
// peer-to-peer handle.
func TestExecRepeatedSessionFailuresHitFailureCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		tr   Transport
		act  p2pAction
	}{
		{"UnacceptedSessionEnd", TransportAuto, p2pAction{endNow: true, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED}},
		{"ChannelFailure", TransportPeerToPeer, p2pAction{failNow: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(1, 0)
			cp.p2pScript = func(int) p2pAction { return tc.act }
			s := cp.handle(t, cp.client(t, tc.tr))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
			var dp *DataPathError
			if !errors.As(err, &dp) {
				t.Fatalf("want a DataPathError, got %v", err)
			}
			if got := cp.openSessionCount(); got != 6 {
				t.Fatalf("%d sessions, want 6 (the sixth failure ends the exec)", got)
			}
		})
	}
}

func TestExecMovesToRelayAfterChannelFailure(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(6, 20*time.Millisecond)
	cp.p2pScript = func(int) p2pAction { return p2pAction{failAfter: 2} }
	s := cp.handle(t, cp.client(t, TransportAuto))
	res, r := runExec(t, s)
	checkLines(t, res, r, 6)
	if res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("transport %v", res.Transport)
	}
	if cp.relayCount("Exec") != 0 || cp.relayCount("AttachExec") < 1 {
		t.Fatalf("relay calls Exec=%d AttachExec=%d; the exec must continue with AttachExec", cp.relayCount("Exec"), cp.relayCount("AttachExec"))
	}
	auths := cp.authRequests()
	if len(auths) == 0 || auths[0].GetOperationKind() != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || len(auths[0].GetRequestDigest()) != 32 {
		t.Fatalf("AuthorizeRelay for the continuation must keep the request digest: %v", auths)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("command ran %d times", cp.agent.totalRuns())
	}
}

func TestConcurrentExecsShareOneSession(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(4, 5*time.Millisecond)
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &recorder{}
			res, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}, OnEvent: r.on})
			if err != nil {
				errs <- err
				return
			}
			if res.ExitCode == nil || *res.ExitCode != 3 || r.stdout.String() != expectedLines(4) {
				errs <- fmt.Errorf("result %+v stdout %q", res, r.stdout.String())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if cp.openSessionCount() != 1 {
		t.Fatalf("%d sessions, want 1", cp.openSessionCount())
	}
}

func TestRelayUnlimitedRotationsReuseGrant(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(12, 5*time.Millisecond)
	cp.relayScript = func(string, int) relayAction { return relayAction{endAfter: 1} }
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 12)
	if cp.relayCount("AttachExec") < 10 {
		t.Fatalf("only %d rotations", cp.relayCount("AttachExec"))
	}
	if n := len(cp.authRequests()); n != 1 {
		t.Fatalf("%d authorizations, want 1 (grant reuse)", n)
	}
}

func TestRelayRenewsGrantAtRotation(t *testing.T) {
	cp := newFakeCP(t)
	// The grant expires before a new stream could end (relay stream limit
	// plus 30 seconds, scaled), so every rotation renews it.
	cp.expiresIn = 900 * time.Millisecond
	cp.agent.run = lineRun(4, 5*time.Millisecond)
	cp.relayScript = func(string, int) relayAction { return relayAction{endAfter: 1} }
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 4)
	auths := cp.authRequests()
	if len(auths) < 4 {
		t.Fatalf("%d authorizations, want one per stream", len(auths))
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for i := 1; i < len(auths); i++ {
		prev := cp.byAuthID[auths[i-1].GetAuthorizationId()]
		if auths[i].ReplacesGrantId == nil || *auths[i].ReplacesGrantId != prev {
			t.Fatalf("renewal %d replaces %v, want %s", i, auths[i].ReplacesGrantId, prev)
		}
		if auths[i].GetAuthorizationId() == auths[i-1].GetAuthorizationId() {
			t.Fatal("a renewal reused the authorization_id of another request")
		}
	}
}

func TestRelayRecoveryAfterAbruptDisconnect(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(6, 10*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" || n == 1 {
			return relayAction{abortAfter: 2}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 6)
	if cp.agent.totalRuns() != 1 {
		t.Fatal("the command ran again")
	}
	auths := cp.authRequests()
	if len(auths) < 2 || auths[1].ReplacesGrantId == nil {
		t.Fatalf("recovery must renew the grant with replaces_grant_id: %v", auths)
	}
}

func TestRelayRecoveryRetriesUnavailable(t *testing.T) {
	for _, reason := range []ownsandv1.SessionEndReason{
		ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST,
		ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID,
		ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED, // no relay connection from the agent
	} {
		t.Run(reason.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			cp.relayScript = func(method string, n int) relayAction {
				if n <= 2 {
					return relayAction{err: detailErr(connect.CodeUnavailable, "unavailable", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, SessionEndReason: reason})}
				}
				return relayAction{}
			}
			s := cp.handle(t, cp.client(t, TransportRelay))
			res, r := runExec(t, s)
			checkLines(t, res, r, 3)
		})
	}
}

func TestRelayRecoveryWhileUnreachable(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE)
	go func() {
		time.Sleep(400 * time.Millisecond)
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	}()
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	if len(cp.authRequests()) < 2 {
		t.Fatal("AuthorizeRelay was not retried")
	}
	auths := cp.authRequests()
	for _, a := range auths[1:] {
		if a.GetAuthorizationId() != auths[0].GetAuthorizationId() {
			t.Fatal("retries of one AuthorizeRelay must reuse its authorization_id")
		}
	}
}

func TestRelayRecoveryWindowEnds(t *testing.T) {
	cp := newFakeCP(t)
	cp.relayScript = func(string, int) relayAction {
		return relayAction{err: grantInvalid()}
	}
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	start := time.Now()
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"echo"}})
	var dp *DataPathError
	if !errors.As(err, &dp) || connect.CodeOf(errors.Unwrap(err)) != connect.CodeUnavailable {
		t.Fatalf("want a DataPathError with unavailable, got %v", err)
	}
	if d := time.Since(start); d > c.timing.recoveryWindow+time.Second {
		t.Fatalf("recovery took %v, longer than the window", d)
	}
}

func TestRelayRevokedCredentialEndsAtOnce(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(20, 50*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{}
	}
	cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
		if n > 1 {
			cp.mu.Lock()
			cp.revoked = true
			cp.mu.Unlock()
		}
		return authAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	start := time.Now()
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
	if connect.CodeOf(errors.Unwrap(err)) != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a revoked credential must end the operation at once")
	}
}

func TestAuthorizeRelayLostResponseReturnsSameGrant(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(2, 0)
	cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
		return authAction{abortAfterGrant: n == 1}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 2)
	auths := cp.authRequests()
	if len(auths) != 2 || auths[0].GetAuthorizationId() != auths[1].GetAuthorizationId() {
		t.Fatalf("authorizations %v", auths)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.grants) != 1 {
		t.Fatalf("%d grants, want 1", len(cp.grants))
	}
}

func TestAuthorizeRelayResourceExhaustedWaitsRetryDelay(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(1, 0)
	var times []time.Time
	cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
		times = append(times, time.Now())
		if n == 1 {
			return authAction{err: detailErr(connect.CodeResourceExhausted, "grant limit", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, RetryAfter: durationpb.New(300 * time.Millisecond)})}
		}
		return authAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 1)
	if len(times) != 2 || times[1].Sub(times[0]) < 300*time.Millisecond {
		t.Fatalf("retry after %v, want at least the retry delay", times[1].Sub(times[0]))
	}
}

func TestRelayStreamRejectedAtCapacityIsRetried(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(2, 0)
	cp.relayScript = func(method string, n int) relayAction {
		if n <= 2 {
			return relayAction{err: connect.NewError(connect.CodeResourceExhausted, errors.New("agent at stream capacity"))}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 2)
	if n := len(cp.authRequests()); n != 1 {
		t.Fatalf("%d authorizations; a capacity rejection keeps the grant", n)
	}
}

func TestStreamWithoutAcceptedIsRetriedWithinRecoveryWindow(t *testing.T) {
	cp := newFakeCP(t)
	cp.relayScript = func(string, int) relayAction { return relayAction{noAccept: true} }
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	start := time.Now()
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"echo"}})
	var dp *DataPathError
	// The window usually ends in a back-off or a stream attempt, and the
	// operation ends with the last stream error. On a slow runner it can end
	// while the renewal before the next attempt is in flight; the SDK then
	// cancels that AuthorizeRelay call and ends with errAuthorizeDeadline
	// (see TestStalledAuthorizeRelayEndsWithTheWindow).
	if !errors.As(err, &dp) || !(errors.Is(err, errNotAccepted) || errors.Is(err, errAuthorizeDeadline)) {
		t.Fatalf("want a DataPathError with errNotAccepted or errAuthorizeDeadline, got %v", err)
	}
	if time.Since(start) > c.timing.recoveryWindow+time.Second {
		t.Fatal("unaccepted streams must end within the recovery window")
	}
	// The early close is a recoverable failure: the SDK retried inside the
	// window and did not give up after the first stream.
	if n := cp.relayCount("Exec") + cp.relayCount("AttachExec"); n < 3 {
		t.Fatalf("%d relay streams, want repeated attempts", n)
	}
	if n := len(cp.authRequests()); n < 3 {
		t.Fatalf("%d authorizations, want a renewal for each retry", n)
	}
}

func TestExecFirstStreamClosedWithoutAcceptedAttachesThenResends(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(2, 0)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" && n == 1 {
			return relayAction{noAccept: true}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 2)
	cp.mu.Lock()
	log := strings.Join(cp.relayLog, ",")
	cp.mu.Unlock()
	// AttachExec first (not_found: the agent never got the request), then
	// the identical ExecRequest with the same exec_id.
	if log != "Exec,AttachExec,Exec" {
		t.Fatalf("relay calls %s, want Exec,AttachExec,Exec", log)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("ran %d times", cp.agent.totalRuns())
	}
	auths := cp.authRequests()
	if len(auths) != 2 || auths[1].ReplacesGrantId == nil || auths[0].GetOperationId() != auths[1].GetOperationId() {
		t.Fatalf("authorizations %v, want one renewal for the same exec_id", auths)
	}
}

func TestExecAttachClosedWithoutAcceptedIsRetried(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 10*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		switch {
		case method == "Exec":
			return relayAction{abortAfter: 1}
		case method == "AttachExec" && n == 1:
			return relayAction{noAccept: true}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	if n := cp.relayCount("AttachExec"); n != 2 {
		t.Fatalf("%d AttachExec calls, want 2", n)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("ran %d times", cp.agent.totalRuns())
	}
}

func TestStalledStreamIsInterrupted(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(4, 10*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{stallAfter: 1}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 4)
	if cp.relayCount("AttachExec") < 1 {
		t.Fatal("the stalled stream was not continued")
	}
}

func TestAcceptedSilentContinuationsKeepClockPaused(t *testing.T) {
	cp := newFakeCP(t)
	// A silent command: the first stream is interrupted, then accepted
	// continuations rotate cleanly without data for longer than the window.
	gate := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		<-gate
		e.write(0, []byte("late\n"))
		e.finish(exitedEvent(0))
	}
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfterAccept: true}
		}
		return relayAction{endAfterTime: 300 * time.Millisecond}
	}
	c := cp.client(t, TransportRelay)
	go func() {
		time.Sleep(2 * c.timing.recoveryWindow)
		close(gate)
	}()
	s := cp.handle(t, c)
	r := &recorder{}
	res, err := s.Exec(context.Background(), ExecParams{Command: []string{"x"}, OnEvent: r.on})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 || r.stdout.String() != "late\n" {
		t.Fatalf("result %+v %q", res, r.stdout.String())
	}
}

func TestExecPausedWaitsForResume(t *testing.T) {
	cp := newFakeCP(t)
	gate := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		e.write(0, []byte("before\n"))
		<-gate
		e.write(0, []byte("after\n"))
		e.finish(exitedEvent(0))
	}
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{}
	}
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	// Pause right after the first chunk; resume later than the window.
	go func() {
		for cp.relayCount("Exec") == 0 {
			time.Sleep(time.Millisecond)
		}
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		time.Sleep(c.timing.recoveryWindow + 300*time.Millisecond)
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		close(gate)
	}()
	r := &recorder{}
	res, err := s.Exec(context.Background(), ExecParams{Command: []string{"x"}, OnEvent: r.on})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || r.stdout.String() != "before\nafter\n" {
		t.Fatalf("result %+v %q", res, r.stdout.String())
	}
}

func TestExecPausedSandboxEnds(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = func(_ []string, e *fakeExec) { e.write(0, []byte("x\n")) }
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{}
	}
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	go func() {
		for cp.relayCount("Exec") == 0 {
			time.Sleep(time.Millisecond)
		}
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		time.Sleep(200 * time.Millisecond)
		cp.mu.Lock()
		cp.endReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
		cp.mu.Unlock()
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	}()
	res, err := s.Exec(context.Background(), ExecParams{Command: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ExecResultSandboxEnded || res.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("result %+v", res)
	}
}

func TestExecBeforeStartedResendsIdenticalRequestAfterNotFound(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(2, 0)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" && n == 1 {
			return relayAction{abortNow: true} // lost before the agent got it
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 2)
	cp.mu.Lock()
	log := strings.Join(cp.relayLog, ",")
	cp.mu.Unlock()
	if log != "Exec,AttachExec,Exec" {
		t.Fatalf("relay calls %s, want Exec,AttachExec,Exec", log)
	}
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("ran %d times", cp.agent.totalRuns())
	}
}

func TestExecPausedBeforeStartedResumesAndSends(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(1, 0)
	cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	c := cp.client(t, TransportRelay)
	go func() {
		time.Sleep(c.timing.recoveryWindow + 200*time.Millisecond)
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	}()
	s := cp.handle(t, c)
	res, r := runExec(t, s)
	checkLines(t, res, r, 1)
}

func TestAttachNotFoundAfterStartedIsUnknownOutcome(t *testing.T) {
	cp := newFakeCP(t)
	wrote := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		e.write(0, []byte("x"))
		close(wrote)
	}
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	go func() {
		<-wrote
		cp.agent.mu.Lock()
		cp.agent.execs = map[string]*fakeExec{}
		cp.agent.mu.Unlock()
	}()
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"x"}})
	var dp *DataPathError
	if !errors.As(err, &dp) || !dp.OutcomeUnknown {
		t.Fatalf("want an unknown outcome, got %v", err)
	}
	if cp.relayCount("Exec") != 1 {
		t.Fatal("the exec was sent again after started")
	}
}

func testTruncation(t *testing.T, tr Transport) {
	cp := newFakeCP(t)
	gate := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		e.write(0, []byte("aaaa"))
		<-gate
		e.write(0, []byte("bbbb"))
		e.drop(0, 6) // the agent keeps only the last bytes
		e.write(0, []byte("cccc"))
		e.finish(exitedEvent(0))
	}
	if tr == TransportRelay {
		cp.relayScript = func(method string, n int) relayAction {
			if method == "Exec" {
				return relayAction{abortAfter: 1}
			}
			return relayAction{}
		}
	} else {
		cp.p2pScript = func(n int) p2pAction {
			if n == 1 {
				return p2pAction{endAfter: 1, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED}
			}
			return p2pAction{}
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(gate)
	}()
	c := cp.client(t, tr)
	s := cp.handle(t, c)
	r := &recorder{}
	res, err := s.Exec(context.Background(), ExecParams{Command: []string{"x"}, OnEvent: r.on})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete {
		t.Fatal("result not marked incomplete")
	}
	want := []ExecEventKind{ExecEventStarted, ExecEventStdout, ExecEventOutputTruncated, ExecEventStdout}
	if fmt.Sprint(r.kinds[:4]) != fmt.Sprint(want) {
		t.Fatalf("events %v, want prefix %v", r.kinds, want)
	}
	if ev := r.events[2]; ev.MissingStdout != (ByteRange{From: 4, To: 6}) {
		t.Fatalf("missing %+v", ev.MissingStdout)
	}
	if got := r.stdout.String(); got != "aaaa\x00\x00bbcccc" {
		t.Fatalf("stdout %q", got)
	}
}

func TestOutputTruncatedRelay(t *testing.T)      { testTruncation(t, TransportRelay) }
func TestOutputTruncatedPeerToPeer(t *testing.T) { testTruncation(t, TransportPeerToPeer) }

func TestExecFailedAndSignal(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = func(cmd []string, e *fakeExec) {
		switch cmd[0] {
		case "missing":
			e.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed{Failed: &ownsandv1.ExecFailed{Reason: ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND, Message: "no such program"}}})
		case "killed":
			e.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_Signal{Signal: 9}, TimedOut: true}}})
		default:
			e.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded{SandboxEnded: &ownsandv1.ExecSandboxEnded{Reason: ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED}}})
		}
	}
	for _, tr := range []Transport{TransportRelay, TransportPeerToPeer} {
		s := cp.handle(t, cp.client(t, tr))
		res, err := s.Exec(context.Background(), ExecParams{Command: []string{"missing"}})
		if err != nil || res.Kind != ExecResultFailed || res.FailureReason != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
			t.Fatalf("%v failed: %+v %v", tr, res, err)
		}
		res, err = s.Exec(context.Background(), ExecParams{Command: []string{"killed"}, Timeout: time.Second})
		if err != nil || res.Kind != ExecResultExited || res.Signal == nil || *res.Signal != 9 || !res.TimedOut || res.ExitCode != nil {
			t.Fatalf("%v signal: %+v %v", tr, res, err)
		}
		res, err = s.Exec(context.Background(), ExecParams{Command: []string{"ended"}})
		if err != nil || res.Kind != ExecResultSandboxEnded || res.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
			t.Fatalf("%v ended: %+v %v", tr, res, err)
		}
	}
}

func TestAttachExecWithAttachGrant(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	res, _ := runExec(t, s)
	// Another program attaches with only the exec_id.
	other := cp.handle(t, cp.client(t, TransportRelay))
	r := &recorder{}
	got, err := other.AttachExec(context.Background(), res.ExecID, AttachParams{OnEvent: r.on})
	if err != nil {
		t.Fatal(err)
	}
	checkLines(t, got, r, 3)
	auths := cp.authRequests()
	last := auths[len(auths)-1]
	if last.GetOperationKind() != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC || len(last.GetRequestDigest()) != 0 {
		t.Fatalf("attach authorization %v", last)
	}
}

func TestNewSaltForEachOperation(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportRelay))
	runExec(t, s)
	runExec(t, s)
	auths := cp.authRequests()
	if len(auths) != 2 || bytes.Equal(auths[0].GetRequestDigest(), auths[1].GetRequestDigest()) {
		t.Fatal("two identical commands must have different digests")
	}
}

func TestElevenOperationsRecoverAtOnce(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(4, 10*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	var wg sync.WaitGroup
	errs := make(chan error, 11)
	for range 11 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &recorder{}
			res, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}, OnEvent: r.on})
			if err == nil && (res.ExitCode == nil || r.stdout.String() != expectedLines(4)) {
				err = fmt.Errorf("result %+v %q", res, r.stdout.String())
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if cp.agent.totalRuns() != 11 {
		t.Fatalf("%d runs", cp.agent.totalRuns())
	}
}

func TestExecValidationBeforeSend(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportRelay))
	if _, err := s.Exec(context.Background(), ExecParams{}); err == nil {
		t.Fatal("empty command accepted")
	}
	big := strings.Repeat("x", 300<<10)
	if _, err := s.Exec(context.Background(), ExecParams{Command: []string{"echo", big}}); err == nil {
		t.Fatal("oversized request accepted")
	}
	if len(cp.authRequests()) != 0 {
		t.Fatal("an invalid request reached the control plane")
	}
}

func TestLostRenewalResponseReturnsInstalledGrant(t *testing.T) {
	cp := newFakeCP(t)
	cp.expiresIn = 900 * time.Millisecond
	cp.agent.run = lineRun(3, 5*time.Millisecond)
	cp.relayScript = func(string, int) relayAction { return relayAction{endAfter: 1} }
	cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
		return authAction{abortAfterGrant: n == 2}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	auths := cp.authRequests()
	if auths[1].GetAuthorizationId() != auths[2].GetAuthorizationId() || auths[2].ReplacesGrantId == nil || *auths[1].ReplacesGrantId != *auths[2].ReplacesGrantId {
		t.Fatal("the retry of a lost renewal must keep its authorization_id and replaces_grant_id")
	}
}

func TestGrantLifetimeUsesExpiresInNotCallerClock(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(4, 5*time.Millisecond)
	cp.relayScript = func(string, int) relayAction { return relayAction{endAfter: 1} }
	// expire_time looks expired on the caller's clock, but expires_in
	// leaves enough time: the grant is reused.
	cp.clockOffset = -time.Hour
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	g := newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, mustID(t), nil)
	if err := g.authorize(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if time.Until(g.expiresAt) < 29*time.Second {
		t.Fatalf("grant lifetime %v not taken from expires_in", time.Until(g.expiresAt))
	}
	res, r := runExec(t, s)
	checkLines(t, res, r, 4)
}

// TestInvalidFrameFailsItsStream checks that a frame that does not follow
// the framing rules (an unknown kind of a newer schema, or a kind that does
// not match its payload) ends the operation at once on the peer-to-peer
// path instead of leaving it waiting for another frame.
func TestInvalidFrameFailsItsStream(t *testing.T) {
	frames := map[string]func(uint64, string) *ownsandv1.DataFrame{
		"unknown kind": func(id uint64, sb string) *ownsandv1.DataFrame {
			return &ownsandv1.DataFrame{StreamId: id, SandboxId: sb, Kind: ownsandv1.DataFrameKind(99)}
		},
		"kind and payload differ": func(id uint64, sb string) *ownsandv1.DataFrame {
			f := datapath.MustNewFrame(id, sb, &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND})
			f.Kind = ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT
			return f
		},
	}
	for name, raw := range frames {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(1, 0)
			cp.agent.setFile("/f", []byte("data"))
			cp.p2pScript = func(int) p2pAction { return p2pAction{raw: raw} }
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var invalid *errInvalidFrame
			_, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
			var dp *DataPathError
			if !errors.As(err, &dp) || !errors.As(err, &invalid) {
				t.Fatalf("exec: want a DataPathError with an invalid frame, got %v", err)
			}
			_, err = s.ReadFileBytes(ctx, "/f")
			if !errors.As(err, &dp) || !errors.As(err, &invalid) {
				t.Fatalf("read: want a DataPathError with an invalid frame, got %v", err)
			}
		})
	}
}

// TestBlockedStreamDoesNotBlockOtherStreams checks that an exec whose
// OnEvent callback waits for another operation on the same handle does not
// deadlock the shared data channel receiver when the exec's stream queue
// fills: the receiver fails the overrun stream, the other operation
// completes, and the exec continues from its delivered offsets.
func TestBlockedStreamDoesNotBlockOtherStreams(t *testing.T) {
	cp := newFakeCP(t)
	// Small output chunks, so the exec sends far more frames than its
	// stream queue holds.
	cp.agent.execChunk = 4
	const lines = 100
	cp.agent.run = lineRun(lines, 0)
	want := []byte("file contents")
	cp.agent.setFile("/f", want)
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := &recorder{}
	var once sync.Once
	var readErr error
	res, err := s.Exec(ctx, ExecParams{Command: []string{"run"}, OnEvent: func(ev ExecEvent) error {
		if ev.Kind == ExecEventStdout {
			once.Do(func() {
				// Let the agent fill the exec's queue before the read.
				time.Sleep(100 * time.Millisecond)
				rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
				defer rcancel()
				got, err := s.ReadFileBytes(rctx, "/f")
				if err == nil && !bytes.Equal(got, want) {
					err = fmt.Errorf("read %q", got)
				}
				readErr = err
			})
		}
		return r.on(ev)
	}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if readErr != nil {
		t.Fatalf("read inside OnEvent: %v", readErr)
	}
	checkLines(t, res, r, lines)
	if cp.agent.totalRuns() != 1 {
		t.Fatalf("command ran %d times", cp.agent.totalRuns())
	}
	if cp.openSessionCount() != 1 {
		t.Fatalf("%d sessions, want 1", cp.openSessionCount())
	}
}

// TestStalledAuthorizeRelayEndsWithTheWindow checks that an AuthorizeRelay
// call whose response never arrives on an open connection ends when its
// retry window ends, both for the first authorization of an operation and
// inside relay recovery, also for a caller without its own deadline.
func TestStalledAuthorizeRelayEndsWithTheWindow(t *testing.T) {
	cases := map[string]struct {
		stallFrom int
		relay     func(method string, n int) relayAction
	}{
		"first authorization": {stallFrom: 1},
		"inside recovery": {stallFrom: 2, relay: func(method string, n int) relayAction {
			if method == "Exec" && n == 1 {
				return relayAction{abortAfter: 1}
			}
			return relayAction{}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(20, 50*time.Millisecond)
			cp.relayScript = tc.relay
			cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
				return authAction{stall: n >= tc.stallFrom}
			}
			c := cp.client(t, TransportRelay)
			s := cp.handle(t, c)
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
				done <- err
			}()
			select {
			case err := <-done:
				var dp *DataPathError
				if !errors.As(err, &dp) || !errors.Is(err, errAuthorizeDeadline) {
					t.Fatalf("want a DataPathError after the stalled AuthorizeRelay, got %v", err)
				}
				if d := time.Since(start); d > c.timing.recoveryWindow+time.Second {
					t.Fatalf("exec ended after %v, longer than the window", d)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("exec did not end after the recovery window")
			}
		})
	}
}

func TestRelayTerminalWireErrorWithCredentialEndsOnce(t *testing.T) {
	cp := newFakeCP(t)
	cp.relayScript = func(string, int) relayAction {
		return relayAction{err: connect.NewError(connect.CodeUnauthenticated, errors.New("rejected "+testAPIKey))}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
	if err == nil || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("exec error: %v", err)
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code %v", connect.CodeOf(err))
	}
	if n := len(cp.authRequests()); n != 1 {
		t.Fatalf("AuthorizeRelay calls = %d, want 1", n)
	}
	cp.mu.Lock()
	n := cp.relayCalls["Exec"]
	cp.mu.Unlock()
	if n != 1 {
		t.Fatalf("Exec streams = %d, want 1", n)
	}
}

func TestRelayUnavailableWithCredentialStillRecovers(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.relayScript = func(method string, n int) relayAction {
		if n <= 1 {
			return relayAction{err: connect.NewError(connect.CodeUnavailable, errors.New("down "+testAPIKey))}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
}

// recoveryDeadlineClient returns a relay client whose recovery window is
// much shorter than the byte-idle limit, so only the recovery deadline can
// end a continuation that the agent does not accept.
func recoveryDeadlineClient(t *testing.T, cp *fakeCP, window time.Duration) *Client {
	t.Helper()
	c := cp.client(t, TransportRelay)
	c.timing.recoveryWindow = window
	c.timing.streamStall = 30 * time.Second
	return c
}

// heldDurations returns how long each held relay stream stayed open. It
// waits briefly for the fake to see the cancellation of every held stream
// that began, because the server handler ends after the client call returns.
func (cp *fakeCP) heldDurations() []time.Duration {
	for range 100 {
		cp.mu.Lock()
		done := len(cp.heldEnds) >= len(cp.heldStarts)
		cp.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var d []time.Duration
	for i := range min(len(cp.heldStarts), len(cp.heldEnds)) {
		d = append(d, cp.heldEnds[i].Sub(cp.heldStarts[i]))
	}
	return d
}

func TestExecContinuationNotAcceptedEndsAtRecoveryDeadline(t *testing.T) {
	for name, act := range map[string]relayAction{
		"blocked receive": {holdOpen: true},
		"keepalives only": {keepalivesOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(5, 20*time.Millisecond)
			cp.relayScript = func(method string, n int) relayAction {
				if method == "Exec" {
					return relayAction{abortAfter: 1}
				}
				return act
			}
			c := recoveryDeadlineClient(t, cp, 300*time.Millisecond)
			s := cp.handle(t, c)
			start := time.Now()
			_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
			elapsed := time.Since(start)
			var dp *DataPathError
			if !errors.As(err, &dp) || !errors.Is(err, errRecoveryDeadline) {
				t.Fatalf("want a DataPathError with errRecoveryDeadline, got %v", err)
			}
			// The error carries the failure that started the recovery: the
			// lost first stream, which has no valid terminal status.
			if !strings.Contains(err.Error(), "last failure") {
				t.Fatalf("error %v does not carry the last recovery failure", err)
			}
			if elapsed > c.timing.recoveryWindow+time.Second {
				t.Fatalf("operation took %v, want about the recovery window %v", elapsed, c.timing.recoveryWindow)
			}
			held := cp.heldDurations()
			if len(held) != 1 || held[0] > c.timing.recoveryWindow+500*time.Millisecond {
				t.Fatalf("held streams %v, want one that the SDK cancelled within the recovery window", held)
			}
			if n := cp.relayCount("AttachExec"); n != 1 {
				t.Fatalf("%d AttachExec calls, want 1: no continuation starts after expiry", n)
			}
		})
	}
}

func TestExecContinuationAcceptedBeforeDeadlineStaysOpenPastIt(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		// Accepted at once, then keepalives for longer than the window.
		return relayAction{acceptedKeepalives: 700 * time.Millisecond}
	}
	c := recoveryDeadlineClient(t, cp, 300*time.Millisecond)
	s := cp.handle(t, c)
	start := time.Now()
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	if time.Since(start) < 2*c.timing.recoveryWindow {
		t.Fatalf("operation took %v; the accepted continuation did not outlive the window", time.Since(start))
	}
	if n := cp.relayCount("AttachExec"); n != 1 {
		t.Fatalf("%d AttachExec calls, want 1", n)
	}
}

func TestExecInterruptedAfterAcceptanceResumesSavedBudget(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.relayScript = func(method string, n int) relayAction {
		switch {
		case method == "Exec":
			return relayAction{abortAfter: 1}
		case n == 1:
			// 300 ms before acceptance use half of the budget; the accepted
			// stream then stays open for longer than the whole window and
			// is lost without data.
			return relayAction{delay: 300 * time.Millisecond, acceptedKeepalives: 800 * time.Millisecond, abortAfterKeepalives: true}
		}
		return relayAction{holdOpen: true}
	}
	c := recoveryDeadlineClient(t, cp, 600*time.Millisecond)
	s := cp.handle(t, c)
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
	if !errors.Is(err, errRecoveryDeadline) {
		t.Fatalf("want errRecoveryDeadline, got %v", err)
	}
	held := cp.heldDurations()
	// A fresh window would hold the second continuation for about 600 ms;
	// the saved budget is what the first continuation left over.
	if len(held) != 1 || held[0] > 480*time.Millisecond {
		t.Fatalf("held streams %v, want one bounded by the saved budget", held)
	}
}

func TestExecCallerCancellationEndsHeldContinuationEarlier(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(5, 20*time.Millisecond)
	cp.relayScript = func(method string, n int) relayAction {
		if method == "Exec" {
			return relayAction{abortAfter: 1}
		}
		return relayAction{holdOpen: true}
	}
	c := recoveryDeadlineClient(t, cp, 5*time.Second)
	s := cp.handle(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRecoveryDeadline) {
		t.Fatalf("want the caller's deadline, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cancellation took %v", time.Since(start))
	}
}

// deadlineStatus is a deadline_exceeded status that the control plane (relay)
// or the agent (p2p error frame) answers for a read or an exec.
func deadlineStatus() error {
	return detailErr(connect.CodeDeadlineExceeded, "server deadline", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, Message: "server deadline"})
}

// deadlineOperations lists the operations that must end at once on a
// deadline_exceeded status. WriteFile is not among them: it retries.
func deadlineOperations(t *testing.T) map[string]struct {
	method string
	run    func(ctx context.Context, s *Sandbox) error
} {
	type op = struct {
		method string
		run    func(ctx context.Context, s *Sandbox) error
	}
	return map[string]op{
		"Exec": {"Exec", func(ctx context.Context, s *Sandbox) error {
			_, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
			return err
		}},
		"AttachExec": {"AttachExec", func(ctx context.Context, s *Sandbox) error {
			_, err := s.AttachExec(ctx, mustID(t), AttachParams{})
			return err
		}},
		"ReadFile": {"ReadFile", func(ctx context.Context, s *Sandbox) error {
			_, err := s.ReadFile(ctx, "/f", io.Discard)
			return err
		}},
		"ReadFileBytes": {"ReadFile", func(ctx context.Context, s *Sandbox) error {
			_, err := s.ReadFileBytes(ctx, "/f")
			return err
		}},
	}
}

// TestDeadlineStatusEndsExecAndReadAtOnce checks that a deadline_exceeded
// status ends an exec or a read at once with a DataPathError, on the relay,
// without a new AuthorizeRelay, and that the exec never runs.
func TestDeadlineStatusEndsExecAndReadAtOnce(t *testing.T) {
	for name, tt := range deadlineOperations(t) {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			cp.agent.setFile("/f", []byte("data"))
			cp.relayScript = func(method string, _ int) relayAction {
				if method == tt.method {
					return relayAction{err: deadlineStatus()}
				}
				return relayAction{}
			}
			s := cp.handle(t, cp.client(t, TransportRelay))
			// A caller deadline far beyond the skew: the status is the
			// control plane's own.
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			start := time.Now()
			err := tt.run(ctx, s)
			var dp *DataPathError
			if !errors.As(err, &dp) || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
				t.Fatalf("want a DataPathError with deadline_exceeded, got %v", err)
			}
			if dp.OutcomeUnknown {
				t.Fatal("a pre-start failure must not report an unknown outcome")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("a server status must not look like the caller's deadline")
			}
			if n := cp.relayCount(tt.method); n != 1 {
				t.Fatalf("%d %s calls, want 1: no retry", n, tt.method)
			}
			if n := len(cp.authRequests()); n != 1 {
				t.Fatalf("%d AuthorizeRelay calls, want 1: no recovery window", n)
			}
			if n := cp.agent.totalRuns(); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
			if d := time.Since(start); d > testTiming().recoveryWindow {
				t.Fatalf("ended after %v, want at once (recovery window %v)", d, testTiming().recoveryWindow)
			}
		})
	}
}

// TestDeadlineStatusEndsExecAndReadAtOnceOnP2P covers the error frames of the
// peer-to-peer path.
func TestDeadlineStatusEndsExecAndReadAtOnceOnP2P(t *testing.T) {
	for name, tt := range deadlineOperations(t) {
		if name == "AttachExec" || name == "ReadFileBytes" {
			// The same loops serve these on the relay test above; one
			// exec and one read cover the shared p2p frame handling.
			continue
		}
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			cp.agent.setFile("/f", []byte("data"))
			cp.p2pScript = func(int) p2pAction {
				return p2pAction{raw: func(id uint64, sb string) *ownsandv1.DataFrame {
					return datapath.MustNewFrame(id, sb, &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, Message: "server deadline"})
				}}
			}
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			start := time.Now()
			err := tt.run(ctx, s)
			var dp *DataPathError
			if !errors.As(err, &dp) || connect.CodeOf(err) != connect.CodeDeadlineExceeded || dp.OutcomeUnknown {
				t.Fatalf("want a DataPathError with deadline_exceeded, got %v", err)
			}
			if d := time.Since(start); d > testTiming().recoveryWindow {
				t.Fatalf("ended after %v, want at once", d)
			}
			if n := cp.agent.totalRuns(); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
		})
	}
}

// TestDeadlineStatusNearCallerDeadlineIsTheCallerDeadline checks the race
// rule: a deadline_exceeded status that arrives within the skew of the
// caller's own deadline is the caller's deadline, so the caller's context
// error is authoritative. The skew is far larger than the caller's timeout,
// so the order does not depend on the speed of the machine.
func TestDeadlineStatusNearCallerDeadlineIsTheCallerDeadline(t *testing.T) {
	for name, tt := range deadlineOperations(t) {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			cp.agent.setFile("/f", []byte("data"))
			cp.relayScript = func(method string, _ int) relayAction {
				if method == tt.method {
					return relayAction{err: deadlineStatus()}
				}
				return relayAction{}
			}
			c := cp.client(t, TransportRelay)
			c.timing.callerDeadlineSkew = 30 * time.Second
			s := cp.handle(t, c)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := tt.run(ctx, s)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want the caller's deadline error, got %v", err)
			}
			var dp *DataPathError
			if errors.As(err, &dp) {
				t.Fatalf("the caller's deadline must not be wrapped: %v", err)
			}
			if n := cp.relayCount(tt.method); n != 1 {
				t.Fatalf("%d %s calls, want 1", n, tt.method)
			}
		})
	}
}
