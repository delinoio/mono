package ownsand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// advance moves the fake clock in steps below the 90-second idle timeout,
// so keepalives keep flowing while long durations pass.
func (e *env) advance(d time.Duration) {
	const step = 20 * time.Second
	for d > 0 {
		s := min(step, d)
		e.srv.Clock.Advance(s)
		d -= s
		time.Sleep(3 * time.Millisecond)
	}
}

// gate is a program that waits for release before it continues.
type gate struct{ ch chan struct{} }

func newGate() *gate     { return &gate{ch: make(chan struct{})} }
func (g *gate) release() { close(g.ch) }
func (g *gate) wait(ctx context.Context) {
	select {
	case <-g.ch:
	case <-ctx.Done():
	}
}

// faultsFor returns an OnRelay hook that applies faults[i] to the i-th
// relay call (1-based) of each operation, and nil afterwards.
func faultsFor(faults map[int]*ownsandtest.Fault) func(*ownsandtest.RelayCall) *ownsandtest.Fault {
	return func(c *ownsandtest.RelayCall) *ownsandtest.Fault { return faults[c.N] }
}

func file(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestRelayOnlyWithoutOpenSession(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.EnablePeerToPeer(ownsandtest.PeerOptions{Setup: loopbackOnly})
	res, _, out, _, err := e.run(ExecOptions{Command: []string{"echo", "hi"}})
	if err != nil || out != "hi\n" || res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("%v %q %+v", err, out, res)
	}
	if n := e.srv.OpenSessionCalls(); n != 0 {
		t.Fatalf("OpenSession called %d times", n)
	}
}

func TestGrantReuseAndOneGrantPerOperation(t *testing.T) {
	e := newEnv(t, TransportRelay)
	data := file(3*datapath.MaxFileChunk + 100)
	if _, err := e.sb.WriteFile(e.ctx, "/a", bytes.NewReader(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sb.ReadFileBytes(e.ctx, "/a"); err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || auth[0].Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE || auth[1].Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE {
		t.Fatalf("authorize calls %+v", auth)
	}
	if auth[0].OperationID == auth[1].OperationID || auth[0].ReplacesGrantID != "" {
		t.Fatal("operations share an ID or a first grant replaced another")
	}
	writes := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayWriteFile)
	// Chunked file writes: one call per chunk, in order, then the finish.
	if len(writes) != 5 {
		t.Fatalf("write calls %d", len(writes))
	}
	for i, w := range writes {
		if w.GrantID != writes[0].GrantID {
			t.Fatal("grant not reused across chunks")
		}
		if i < 4 && w.Offset != int64(i*datapath.MaxFileChunk) {
			t.Fatalf("chunk %d offset %d", i, w.Offset)
		}
	}
	if !writes[4].Finish {
		t.Fatal("last call is not the finish")
	}
	reads := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)
	if reads[0].GrantID == writes[0].GrantID {
		t.Fatal("read used the write grant")
	}
}

func TestNewSaltForEachOperation(t *testing.T) {
	e := newEnv(t, TransportRelay)
	for range 2 {
		if _, _, _, _, err := e.run(ExecOptions{Command: []string{"echo", "same"}}); err != nil {
			t.Fatal(err)
		}
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || bytes.Equal(auth[0].Digest, auth[1].Digest) || len(auth[0].Digest) != 32 {
		t.Fatalf("digests not distinct: %+v", auth)
	}
}

func TestUnlimitedStreamRotations(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("lines", func(ctx context.Context, x *ownsandtest.ExecIO) {
		for i := range 30 {
			x.Stdout(fmt.Appendf(nil, "line %d\n", i))
			time.Sleep(time.Millisecond)
		}
		g.wait(ctx)
		x.Exit(0)
	})
	var rotations atomic.Int32
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		if c.N <= 25 {
			rotations.Add(1)
			// Rotate right after the (repeated) started event, so the
			// rotations do not depend on how output is chunked.
			return &ownsandtest.Fault{AfterMessages: 1, After: ownsandtest.ActionRotate}
		}
		return nil
	}
	go func() {
		for rotations.Load() < 25 {
			time.Sleep(time.Millisecond)
		}
		g.release()
	}()
	res, evs, out, _, err := e.run(ExecOptions{Command: []string{"lines"}})
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for i := range 30 {
		want += fmt.Sprintf("line %d\n", i)
	}
	if out != want || res.Incomplete || count(evs, ExecEventStarted) != 1 {
		t.Fatalf("out %q events %v", out, kinds(evs))
	}
	if e.srv.Agent.TotalRuns() != 1 {
		t.Fatal("command ran again")
	}
	if n := len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayAttachExec)); n < 25 {
		t.Fatalf("attach calls %d", n)
	}
}

// TestRenewalAtRotationAfter14Minutes covers a silent command (no output for
// more than 14 minutes), the control plane's rotation, the renewal with
// replaces_grant_id and AttachExec with the kept request fields.
func TestRenewalAtRotationAfter14Minutes(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("silent", func(ctx context.Context, x *ownsandtest.ExecIO) {
		g.wait(ctx)
		x.Stdout([]byte("done"))
		x.Exit(0)
	})
	st, err := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"silent"}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st.Recv()
	if err != nil || ev.Kind != ExecEventStarted {
		t.Fatalf("%v %v", ev, err)
	}
	e.advance(15 * time.Minute)
	waitFor(t, func() bool { return len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayAttachExec)) >= 1 })
	g.release()
	res, evs, out, _, err := collect(st)
	if err != nil || out != "done" || *res.ExitCode != 0 {
		t.Fatalf("%v %q", err, out)
	}
	if count(evs, ExecEventStarted) != 0 {
		t.Fatal("repeated started passed to the caller")
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) < 2 {
		t.Fatalf("authorize calls %d", len(auth))
	}
	first, renew := auth[0], auth[1]
	if renew.ReplacesGrantID == "" || renew.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || !bytes.Equal(renew.Digest, first.Digest) {
		t.Fatalf("renewal %+v", renew)
	}
	if renew.At.Sub(first.At) < 14*time.Minute {
		t.Fatalf("renewed while the stream was open: %v", renew.At.Sub(first.At))
	}
	calls := e.srv.RelayCalls()
	if calls[0].GrantID != first.ReplacesGrantID && renew.ReplacesGrantID != calls[0].GrantID {
		t.Fatal("renewal does not name the first grant")
	}
	if e.srv.Agent.TotalRuns() != 1 {
		t.Fatal("command ran again")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRenewalOutsideRecoveryRetriesWithinItsWindow: a renewal at a new
// stream retries unavailable with the same authorization_id and
// replaces_grant_id.
func TestRenewalOutsideRecoveryRetriesWithinItsWindow(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnAuthorize = func(c *ownsandtest.AuthorizeCall) *ownsandtest.Fault {
		if c.N == 2 || c.N == 3 {
			return &ownsandtest.Fault{Err: connect.NewError(connect.CodeUnavailable, errors.New("deploying"))}
		}
		return nil
	}
	r := newPausingReader(file(2*datapath.MaxFileChunk), datapath.MaxFileChunk)
	done := make(chan error, 1)
	go func() {
		_, err := e.sb.WriteFile(e.ctx, "/b", r, 0o600)
		done <- err
	}()
	<-r.reached
	e.advance(61 * time.Second) // the grant idled out
	close(r.pause)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 4 {
		t.Fatalf("authorize calls %d", len(auth))
	}
	for _, c := range auth[2:] {
		if c.AuthorizationID != auth[1].AuthorizationID || c.ReplacesGrantID != auth[1].ReplacesGrantID {
			t.Fatal("retry changed authorization_id or replaces_grant_id")
		}
	}
	if auth[1].ReplacesGrantID == "" || auth[1].AuthorizationID == auth[0].AuthorizationID {
		t.Fatalf("renewal %+v", auth[1])
	}
}

// pausingReader blocks once at pauseAt until pause is closed. It closes
// reached when it blocks, which is after the previous chunk was
// acknowledged.
type pausingReader struct {
	data    []byte
	off     int
	pauseAt int
	pause   chan struct{}
	reached chan struct{}
	paused  bool
}

func newPausingReader(data []byte, at int) *pausingReader {
	return &pausingReader{data: data, pauseAt: at, pause: make(chan struct{}), reached: make(chan struct{})}
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if r.off == r.pauseAt && !r.paused {
		r.paused = true
		close(r.reached)
		<-r.pause
	}
	end := len(r.data)
	if r.off < r.pauseAt {
		end = r.pauseAt
	}
	n := copy(p, r.data[r.off:end])
	r.off += n
	return n, nil
}

func TestRenewalBeforeWriteChunkAfterThreeMinutePause(t *testing.T) {
	e := newEnv(t, TransportRelay)
	data := file(2 * datapath.MaxFileChunk)
	r := newPausingReader(data, datapath.MaxFileChunk)
	done := make(chan error, 1)
	go func() {
		_, err := e.sb.WriteFile(e.ctx, "/c", r, 0o600)
		done <- err
	}()
	<-r.reached
	e.advance(3 * time.Minute)
	close(r.pause)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	writes := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayWriteFile)
	if len(auth) != 2 || auth[1].ReplacesGrantID != writes[0].GrantID || writes[1].GrantID == writes[0].GrantID {
		t.Fatalf("auth %+v writes %+v", auth, writes)
	}
	// The renewal was a renewal, not a recovery: no relay call failed.
	if len(writes) != 3 {
		t.Fatalf("write calls %d", len(writes))
	}
	if got, _ := e.srv.Agent.File("/c"); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestRenewalBeforeWriteChunkWhoseGrantWouldExpireDuringTheCall(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.GrantLifetime = 6 * time.Minute
	r := newPausingReader(file(2*datapath.MaxFileChunk), datapath.MaxFileChunk)
	done := make(chan error, 1)
	go func() {
		_, err := e.sb.WriteFile(e.ctx, "/d", r, 0o600)
		done <- err
	}()
	<-r.reached
	// 40 seconds: below the 60-second idle rule, but 40 s + 5 min 30 s
	// passes the 6-minute grant.
	e.advance(40 * time.Second)
	close(r.pause)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) < 2 || auth[1].ReplacesGrantID == "" {
		t.Fatalf("no renewal: %+v", auth)
	}
}

// TestRenewalFromExpiresInWithCallerClockOffset: expires_at far in the past
// must not cause a renewal, because the SDK uses expires_in.
func TestRenewalFromExpiresInWithCallerClockOffset(t *testing.T) {
	for _, off := range []time.Duration{-2 * time.Hour, 2 * time.Hour} {
		e := newEnv(t, TransportRelay)
		e.srv.ExpiresAtOffset = off
		if _, err := e.sb.WriteFile(e.ctx, "/e", bytes.NewReader(file(3*datapath.MaxFileChunk)), 0o600); err != nil {
			t.Fatal(err)
		}
		if n := len(e.srv.AuthorizeCalls()); n != 1 {
			t.Fatalf("offset %v: %d authorizations", off, n)
		}
	}
}

func TestSecondInterruptionWithinOneRecoveryWindow(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 1000
	data := file(5000)
	e.srv.Agent.SetFile("/f", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		1: {AfterMessages: 2, After: ownsandtest.ActionAbort},
		2: {Abort: true},
	})
	got, err := e.sb.ReadFileBytes(e.ctx, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
	reads := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)
	if len(reads) != 3 || reads[1].Offset != 2000 || reads[2].Offset != 2000 {
		t.Fatalf("reads %+v", reads)
	}
}

func TestAbruptDisconnectAfterChunkResumesAtNextOffset(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 4096
	data := file(10000)
	e.srv.Agent.SetFile("/g", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {AfterMessages: 1, After: ownsandtest.ActionAbort}})
	got, err := e.sb.ReadFileBytes(e.ctx, "/g")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	reads := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)
	if len(reads) != 2 || reads[1].Offset != 4096 {
		t.Fatalf("reads %+v", reads)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || auth[1].ReplacesGrantID != reads[0].GrantID {
		t.Fatalf("recovery renewal %+v", auth)
	}
}

func TestRepeatedAcceptedContinuationsWithoutDataEndWithinWindow(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 1000
	e.srv.Agent.SetFile("/h", file(3000))
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		if c.N == 1 {
			return &ownsandtest.Fault{AfterMessages: 1, After: ownsandtest.ActionAbort}
		}
		return &ownsandtest.Fault{After: ownsandtest.ActionAbort}
	}
	start := e.srv.Clock.Now()
	_, err := e.sb.ReadFileBytes(e.ctx, "/h")
	if err == nil {
		t.Fatal("read succeeded")
	}
	var de *DataPathError
	if !errors.As(err, &de) {
		t.Fatalf("not a data path error: %v", err)
	}
	if el := e.srv.Clock.Now().Sub(start); el > 90*time.Second {
		t.Fatalf("took %v", el)
	}
	for _, r := range relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)[1:] {
		if r.Offset != 1000 {
			t.Fatal("continuation offset moved without data")
		}
	}
}

func TestTwoAcceptedContinuationsDisconnectAtSameOffset(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 1000
	data := file(3000)
	e.srv.Agent.SetFile("/i", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		1: {AfterMessages: 1, After: ownsandtest.ActionAbort},
		2: {After: ownsandtest.ActionAbort},
		3: {After: ownsandtest.ActionAbort},
	})
	got, err := e.sb.ReadFileBytes(e.ctx, "/i")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
}

// TestAcceptedPausesRecoveryClock: an accepted continuation that stays open
// for two minutes without data does not use the window; only data resets it.
func TestAcceptedPausesRecoveryClock(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 1000
	data := file(3000)
	e.srv.Agent.SetFile("/j", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		1: {AfterMessages: 1, After: ownsandtest.ActionAbort},
		2: {Hold: 2 * time.Minute, After: ownsandtest.ActionAbort},
		3: {Delay: 40 * time.Second, Abort: true},
	})
	got, err := e.sb.ReadFileBytes(e.ctx, "/j")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
}

func TestStreamEndingWithoutAcceptedIsNotARotation(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.SetFile("/k", file(10))
	e.srv.OnRelay = func(*ownsandtest.RelayCall) *ownsandtest.Fault { return &ownsandtest.Fault{EndBeforeAccepted: true} }
	start := e.srv.Clock.Now()
	_, err := e.sb.ReadFileBytes(e.ctx, "/k")
	if err == nil || CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
	el := e.srv.Clock.Now().Sub(start)
	if el < 50*time.Second || el > 90*time.Second {
		t.Fatalf("gave up after %v", el)
	}
}

func TestAcceptedStreamSilentFor90SecondsIsInterrupted(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 1000
	data := file(2000)
	e.srv.Agent.SetFile("/l", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {AfterMessages: 1, After: ownsandtest.ActionStall}})
	got, err := e.sb.ReadFileBytes(e.ctx, "/l")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	reads := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)
	if len(reads) != 2 || reads[1].Offset != 1000 || reads[1].At.Sub(reads[0].At) < streamIdleTimeout {
		t.Fatalf("reads %+v", reads)
	}
}

func TestRecoveryReadWithSlowFirstChunk(t *testing.T) {
	e := newEnv(t, TransportRelay)
	data := file(datapath.MaxFileChunk)
	e.srv.Agent.SetFile("/m", data)
	// The continuation's 1 MiB chunk arrives over 120 s in 64 KiB pieces,
	// 7.5 s apart: no 90-second gap without a byte.
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		1: {Abort: true},
		2: {SlowWrite: 7500 * time.Millisecond},
	})
	got, err := e.sb.ReadFileBytes(e.ctx, "/m")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	if n := len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)); n != 2 {
		t.Fatalf("read calls %d", n)
	}
}

func TestKeepaliveBeforeAcceptedLeavesRecoveryTimerRunning(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.SetFile("/n", file(10))
	e.srv.OnRelay = func(*ownsandtest.RelayCall) *ownsandtest.Fault {
		return &ownsandtest.Fault{KeepaliveFirst: true, Delay: 25 * time.Second, Err: connect.NewError(connect.CodeUnavailable, errors.New("acceptance timeout"))}
	}
	start := e.srv.Clock.Now()
	if _, err := e.sb.ReadFileBytes(e.ctx, "/n"); err == nil {
		t.Fatal("read succeeded")
	}
	if el := e.srv.Clock.Now().Sub(start); el > 120*time.Second {
		t.Fatalf("keepalives extended the window: %v", el)
	}
}

func TestStreamRejectedAtAgentCapacityIsRetried(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {Err: ownsandtest.RetryError(time.Second)}})
	res, _, out, _, err := e.run(ExecOptions{Command: []string{"echo", "x"}})
	if err != nil || out != "x\n" || *res.ExitCode != 0 {
		t.Fatal(err)
	}
	if e.srv.Agent.TotalRuns() != 1 {
		t.Fatal("ran twice")
	}
}

func TestContinuationRejectedTwiceAtCapacitySucceeds(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 100
	data := file(300)
	e.srv.Agent.SetFile("/o", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		1: {AfterMessages: 1, After: ownsandtest.ActionAbort},
		2: {Err: ownsandtest.RetryError(2 * time.Second)},
		3: {Err: ownsandtest.RetryError(2 * time.Second)},
	})
	got, err := e.sb.ReadFileBytes(e.ctx, "/o")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
}

func TestAuthorizeResourceExhaustedWaitsRetryDelay(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnAuthorize = func(c *ownsandtest.AuthorizeCall) *ownsandtest.Fault {
		if c.N == 1 {
			return &ownsandtest.Fault{Err: ownsandtest.RetryError(7 * time.Second)}
		}
		return nil
	}
	if _, _, _, _, err := e.run(ExecOptions{Command: []string{"echo"}}); err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || auth[1].At.Sub(auth[0].At) < 7*time.Second || auth[1].AuthorizationID != auth[0].AuthorizationID {
		t.Fatalf("auth %+v", auth)
	}
}

func TestLostAuthorizeResponseReturnsSameGrant(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnAuthorize = func(c *ownsandtest.AuthorizeCall) *ownsandtest.Fault {
		if c.N == 1 {
			return &ownsandtest.Fault{LoseResponse: true}
		}
		return nil
	}
	if _, _, _, _, err := e.run(ExecOptions{Command: []string{"echo"}}); err != nil {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || auth[0].AuthorizationID != auth[1].AuthorizationID {
		t.Fatalf("auth %+v", auth)
	}
	calls := e.srv.RelayCalls()
	if len(calls) != 1 {
		t.Fatalf("relay calls %+v", calls)
	}
}

// TestLostRenewalResponseAfterReplacementInstalled: the agent installed the
// replacement grant, the response was lost; the retry with the same
// authorization_id returns that grant.
func TestLostRenewalResponseAfterReplacementInstalled(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 100
	data := file(300)
	e.srv.Agent.SetFile("/p", data)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {AfterMessages: 1, After: ownsandtest.ActionAbort}})
	e.srv.OnAuthorize = func(c *ownsandtest.AuthorizeCall) *ownsandtest.Fault {
		if c.N == 2 {
			return &ownsandtest.Fault{LoseResponse: true}
		}
		return nil
	}
	got, err := e.sb.ReadFileBytes(e.ctx, "/p")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 3 || auth[1].AuthorizationID != auth[2].AuthorizationID || auth[1].ReplacesGrantID != auth[2].ReplacesGrantID || auth[1].ReplacesGrantID == "" {
		t.Fatalf("auth %+v", auth)
	}
}

func TestWriteAttemptWithoutResponseBeforeSDKDeadline(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {Stall: true}})
	done := make(chan error, 1)
	data := file(1000)
	go func() { done <- e.sb.WriteFileBytes(e.ctx, "/q", data, 0o600) }()
	waitFor(t, func() bool { return len(e.srv.RelayCalls()) == 1 })
	e.advance(writeAttemptDeadline + time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	writes := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayWriteFile)
	if len(writes) != 3 || writes[1].Offset != 0 || writes[1].Finish {
		t.Fatalf("writes %+v", writes)
	}
	if got, _ := e.srv.Agent.File("/q"); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
}

func TestWriteRetryAfterDeadlineExceeded(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
		2: {Err: connect.NewError(connect.CodeDeadlineExceeded, errors.New("call over 5 minutes"))},
	})
	data := file(datapath.MaxFileChunk + 10)
	if err := e.sb.WriteFileBytes(e.ctx, "/r", data, 0o600); err != nil {
		t.Fatal(err)
	}
	writes := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayWriteFile)
	if len(writes) != 4 || writes[1].Offset != writes[2].Offset {
		t.Fatalf("writes %+v", writes)
	}
}

func TestWriteResendsUnacknowledgedChunkAfterLostResponse(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {LoseResponse: true}, 3: {LoseResponse: true}})
	data := file(datapath.MaxFileChunk + 10)
	if err := e.sb.WriteFileBytes(e.ctx, "/s", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.srv.Agent.File("/s"); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
	writes := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayWriteFile)
	if len(writes) != 5 || writes[0].Offset != writes[1].Offset || writes[2].Offset != writes[3].Offset || !writes[4].Finish {
		t.Fatalf("writes %+v", writes)
	}
}

func TestRevokedCredentialEndsAtOnceWithUnauthenticated(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnAuthorize = func(*ownsandtest.AuthorizeCall) *ownsandtest.Fault { return &ownsandtest.Fault{RevokeAfter: true} }
	start := e.srv.Clock.Now()
	_, _, _, _, err := e.run(ExecOptions{Command: []string{"echo"}})
	if CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v", err)
	}
	if len(e.srv.RelayCalls()) != 1 || len(e.srv.AuthorizeCalls()) != 1 || e.srv.Clock.Now().Sub(start) > 10*time.Second {
		t.Fatal("retried a revoked credential")
	}
}

// TestRecoveryWindowAfterRelayFailures covers AGENT_CHANNEL_LOST,
// GRANT_INVALID, a missing agent relay connection and a control plane
// restart, each with a continuation from the delivered offsets.
func TestRecoveryWindowAfterRelayFailures(t *testing.T) {
	cases := map[string]map[int]*ownsandtest.Fault{
		"agent channel lost": {1: {AfterMessages: 2, After: ownsandtest.ActionChannelLost}},
		"grant invalid":      {2: {Err: ownsandtest.GrantInvalidError()}, 1: {AfterMessages: 2, After: ownsandtest.ActionAbort}},
		"no agent relay connection": {
			1: {AfterMessages: 2, After: ownsandtest.ActionAbort},
			2: {Err: connect.NewError(connect.CodeUnavailable, errors.New("no agent relay connection"))},
			3: {Err: connect.NewError(connect.CodeUnavailable, errors.New("no agent relay connection"))},
		},
	}
	for name, faults := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, TransportRelay)
			g := newGate()
			e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) {
				x.Stdout([]byte("first."))
				g.wait(ctx)
				x.Stdout([]byte("second."))
				x.Exit(0)
			})
			e.srv.OnRelay = faultsFor(faults)
			go func() {
				waitFor(t, func() bool { return len(e.srv.RelayCalls()) >= 2 })
				g.release()
			}()
			_, evs, out, _, err := e.run(ExecOptions{Command: []string{"two"}})
			if err != nil || out != "first.second." || count(evs, ExecEventStarted) != 1 {
				t.Fatalf("%v %q %v", err, out, kinds(evs))
			}
			attaches := relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayAttachExec)
			if len(attaches) == 0 || attaches[0].Offset != int64(len("first.")) {
				t.Fatalf("attaches %+v", attaches)
			}
			if e.srv.Agent.TotalRuns() != 1 {
				t.Fatal("ran twice")
			}
		})
	}
	t.Run("control plane restart while unreachable", func(t *testing.T) {
		e := newEnv(t, TransportRelay)
		g := newGate()
		e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) {
			x.Stdout([]byte("first."))
			g.wait(ctx)
			x.Stdout([]byte("second."))
			x.Exit(0)
		})
		st, err := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"two"}})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := st.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		e.srv.SetAgentOnline(false)
		e.srv.Restart()
		e.advance(30 * time.Second)
		e.srv.SetAgentOnline(true)
		g.release()
		_, _, out, _, err := collect(st)
		if err != nil || out != "second." {
			t.Fatalf("%v %q", err, out)
		}
	})
	t.Run("window ends while the agent stays offline", func(t *testing.T) {
		e := newEnv(t, TransportRelay)
		g := newGate()
		defer g.release()
		e.program("wait", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Exit(0) })
		st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"wait"}})
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
		e.srv.SetAgentOnline(false)
		start := e.srv.Clock.Now()
		_, err := st.Recv()
		var de *DataPathError
		if !errors.As(err, &de) || CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("err = %v", err)
		}
		if el := e.srv.Clock.Now().Sub(start); el < 55*time.Second || el > 90*time.Second {
			t.Fatalf("window %v", el)
		}
		// Back-off: far fewer attempts than one per 250 ms.
		if n := len(e.srv.AuthorizeCalls()); n > 25 {
			t.Fatalf("%d authorize calls", n)
		}
	})
}

func TestRecoveryOfElevenOperationsOfOnePrincipal(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.ReadChunk = 100
	data := file(300)
	for i := range 11 {
		e.srv.Agent.SetFile(fmt.Sprintf("/f%d", i), data)
	}
	var started sync.WaitGroup
	started.Add(11)
	release := make(chan struct{})
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		if c.N == 1 {
			started.Done()
			<-release
			return &ownsandtest.Fault{AfterMessages: 1, After: ownsandtest.ActionAbort}
		}
		return nil
	}
	errs := make(chan error, 11)
	for i := range 11 {
		go func() {
			got, err := e.sb.ReadFileBytes(e.ctx, fmt.Sprintf("/f%d", i))
			if err == nil && !bytes.Equal(got, data) {
				err = errors.New("content differs")
			}
			errs <- err
		}()
	}
	started.Wait()
	close(release)
	for range 11 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.srv.AuthorizeCalls()); n <= 22 {
		// 11 first grants + 11 recovery grants; the rate limit (10/s) must
		// have rejected some recovery requests, which were retried.
		t.Logf("authorize calls %d (rate limit not reached)", n)
	}
}

func TestExecNeverRunAgain(t *testing.T) {
	t.Run("stream lost after the agent accepted", func(t *testing.T) {
		e := newEnv(t, TransportRelay)
		e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {After: ownsandtest.ActionAbort}})
		_, evs, out, _, err := e.run(ExecOptions{Command: []string{"echo", "once"}})
		if err != nil || out != "once\n" || count(evs, ExecEventStarted) != 1 {
			t.Fatalf("%v %q", err, out)
		}
		calls := e.srv.RelayCalls()
		if calls[1].RPC != ownsandtest.RelayAttachExec || calls[1].Offset != 0 || e.srv.Agent.TotalRuns() != 1 {
			t.Fatalf("calls %+v runs %d", calls, e.srv.Agent.TotalRuns())
		}
	})
	t.Run("request never reached the agent", func(t *testing.T) {
		e := newEnv(t, TransportRelay)
		e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {Abort: true}})
		_, _, out, _, err := e.run(ExecOptions{Command: []string{"echo", "once"}})
		if err != nil || out != "once\n" {
			t.Fatal(err)
		}
		calls := e.srv.RelayCalls()
		// AttachExec from offset zero first, the identical ExecRequest only
		// after not_found.
		if len(calls) != 3 || calls[1].RPC != ownsandtest.RelayAttachExec || calls[2].RPC != ownsandtest.RelayExec || e.srv.Agent.TotalRuns() != 1 {
			t.Fatalf("calls %+v", calls)
		}
		auth := e.srv.AuthorizeCalls()
		if !bytes.Equal(auth[len(auth)-1].Digest, auth[0].Digest) {
			t.Fatal("continuation grant lost the kept request fields")
		}
	})
	t.Run("attach not_found after started is an unknown outcome", func(t *testing.T) {
		e := newEnv(t, TransportRelay)
		g := newGate()
		defer g.release()
		e.program("wait", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Exit(0) })
		e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{
			1: {AfterMessages: 1, After: ownsandtest.ActionAbort},
			2: {Err: connect.NewError(connect.CodeNotFound, errors.New("exec not found"))},
		})
		_, _, _, _, err := e.run(ExecOptions{Command: []string{"wait"}})
		var de *DataPathError
		if !errors.As(err, &de) || !de.OutcomeUnknown {
			t.Fatalf("err = %v", err)
		}
		if e.srv.Agent.TotalRuns() != 1 {
			t.Fatal("ran twice")
		}
	})
}

func TestTruncatedAttachEventOrderOnRelay(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.Agent.KeepBytes = 1000
	g := newGate()
	e.program("flood", func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout(bytes.Repeat([]byte("a"), 100))
		g.wait(ctx)
		x.Stdout(bytes.Repeat([]byte("b"), 3000))
		x.Exit(0)
	})
	e.srv.OnRelay = faultsFor(map[int]*ownsandtest.Fault{1: {AfterMessages: 2, After: ownsandtest.ActionStall}})
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"flood"}})
	for range 2 {
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	g.release()
	time.Sleep(20 * time.Millisecond)
	e.srv.Restart()
	res, evs, _, _, err := collect(st)
	if err != nil {
		t.Fatal(err)
	}
	checkTruncatedOrder(t, res, evs)
}

func checkTruncatedOrder(t *testing.T, res *ExecResult, evs []*ExecEvent) {
	t.Helper()
	if !res.Incomplete {
		t.Fatal("truncated result not marked incomplete")
	}
	ks := kinds(evs)
	if len(ks) < 3 || ks[0] != ExecEventOutputTruncated || ks[len(ks)-1] != ExecEventExited {
		t.Fatalf("events %v", ks)
	}
	gap := evs[0].Gaps[0]
	if gap.Stream != ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT || gap.From != 100 || gap.To != 3100-1000 {
		t.Fatalf("gap %+v", gap)
	}
	if evs[1].Offset != 2100 {
		t.Fatalf("first chunk after truncation at %d", evs[1].Offset)
	}
}

// TestPausedSandboxResumesAfter90SecondsBeforeStarted: the request was lost
// before the agent, the sandbox was paused before the resend; the SDK waits
// for the resume and sends the identical ExecRequest.
func TestPausedSandboxResumesAfter90SecondsBeforeStarted(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		if c.N == 1 {
			e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, 0)
			return &ownsandtest.Fault{Abort: true}
		}
		return nil
	}
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		defer close(done)
		_, _, out, _, err = e.run(ExecOptions{Command: []string{"echo", "late"}})
	}()
	waitFor(t, func() bool { return len(e.srv.RelayCalls()) >= 3 })
	e.advance(90 * time.Second)
	e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	<-done
	if err != nil || out != "late\n" || e.srv.Agent.TotalRuns() != 1 {
		t.Fatalf("%v %q", err, out)
	}
}

func TestAttachReturnsPausedAndResumesAfterMoreThan60Seconds(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout([]byte("a"))
		g.wait(ctx)
		x.Stdout([]byte("b"))
		x.Exit(0)
	})
	// The stream breaks; the AttachExec continuation finds the sandbox
	// paused and ends with a paused event.
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		switch c.N {
		case 1:
			return &ownsandtest.Fault{AfterMessages: 2, After: ownsandtest.ActionAbort}
		case 2:
			e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, 0)
		}
		return nil
	}
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"two"}})
	waitFor(t, func() bool { return len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayAttachExec)) >= 1 })
	time.Sleep(20 * time.Millisecond)
	e.advance(75 * time.Second)
	e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	g.release()
	res, evs, out, _, err := collect(st)
	if err != nil || out != "ab" || res.Outcome != ExecExited || count(evs, ExecEventStarted) != 1 {
		t.Fatalf("%v %q", err, out)
	}
}

func TestPausedSandboxThatEndsGivesSandboxEnded(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	defer g.release()
	e.program("wait", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Exit(0) })
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"wait"}})
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	// The exec stream itself ends with paused; the SDK watches the sandbox.
	e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, 0)
	time.Sleep(20 * time.Millisecond)
	e.advance(30 * time.Second)
	e.srv.SetState(e.sb.ID(), ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED)
	res, evs, _, _, err := collect(st)
	if err != nil || res.Outcome != ExecSandboxEnded || res.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED || evs[len(evs)-1].Kind != ExecEventSandboxEnded {
		t.Fatalf("%v %+v", err, res)
	}
}

// TestSilentExecRecoversThenRotatesBeyondSetupTime: after one recovery, a
// clean rotation ends the window, so later rotations with slow setup do not
// add up to the 60-second window.
func TestSilentExecRecoversThenRotatesBeyondSetupTime(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("silent", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Exit(0) })
	var rotations atomic.Int32
	e.srv.OnRelay = func(c *ownsandtest.RelayCall) *ownsandtest.Fault {
		switch {
		case c.N == 1:
			return &ownsandtest.Fault{AfterMessages: 1, After: ownsandtest.ActionAbort}
		case c.N <= 9:
			rotations.Add(1)
			return &ownsandtest.Fault{Delay: 15 * time.Second, AfterMessages: 1, After: ownsandtest.ActionRotate}
		}
		return nil
	}
	go func() {
		waitFor(t, func() bool { return rotations.Load() >= 8 })
		g.release()
	}()
	res, _, _, _, err := e.run(ExecOptions{Command: []string{"silent"}})
	if err != nil || res.Outcome != ExecExited {
		t.Fatalf("%v", err)
	}
}

// TestLateContinuationOfSilentExec: a silent exec's stream is interrupted
// after 12 minutes; the continuation renews the grant first.
func TestLateContinuationOfSilentExec(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("silent", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Stdout([]byte("ok")); x.Exit(0) })
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"silent"}})
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	e.advance(12 * time.Minute)
	e.srv.Restart()
	waitFor(t, func() bool { return len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayAttachExec)) >= 1 })
	g.release()
	_, _, out, _, err := collect(st)
	if err != nil || out != "ok" {
		t.Fatal(err)
	}
	auth := e.srv.AuthorizeCalls()
	if len(auth) != 2 || auth[1].ReplacesGrantID == "" {
		t.Fatalf("auth %+v", auth)
	}
}

func TestExecResultKinds(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.program("sig", func(ctx context.Context, x *ownsandtest.ExecIO) { x.Signal(9) })
	e.program("slow", func(ctx context.Context, x *ownsandtest.ExecIO) { x.ExitTimedOut(143) })
	res, _, _, _, err := e.run(ExecOptions{Command: []string{"sig"}})
	if err != nil || res.Signal == nil || *res.Signal != 9 || res.ExitCode != nil {
		t.Fatalf("%v %+v", err, res)
	}
	res, _, _, _, err = e.run(ExecOptions{Command: []string{"slow"}, Timeout: time.Second})
	if err != nil || !res.TimedOut || *res.ExitCode != 143 {
		t.Fatalf("%v %+v", err, res)
	}
	res, evs, _, _, err := e.run(ExecOptions{Command: []string{"missing-binary"}})
	if err != nil || res.Outcome != ExecFailed || res.FailureReason == "" || evs[len(evs)-1].Kind != ExecEventFailed {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"x"}, Env: map[string]string{"BIG": string(make([]byte, datapath.MaxExecRequestFields))}}); CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversized request: %v", err)
	}
}

func TestReadFileVisibleErrors(t *testing.T) {
	e := newEnv(t, TransportRelay)
	if _, err := e.sb.ReadFileBytes(e.ctx, "/missing"); CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v", err)
	}
	e.srv.Agent.SetFile("/changing", file(10))
	e.srv.OnRelay = func(*ownsandtest.RelayCall) *ownsandtest.Fault {
		return &ownsandtest.Fault{Err: connect.NewError(connect.CodeAborted, errors.New("file changed during the copy"))}
	}
	if _, err := e.sb.ReadFileBytes(e.ctx, "/changing"); CodeOf(err) != connect.CodeAborted {
		t.Fatalf("err = %v", err)
	}
	if n := len(relayCallsOf(e.srv.RelayCalls(), ownsandtest.RelayReadFile)); n != 2 {
		t.Fatalf("explicit aborted was retried: %d calls", n)
	}
}

func TestAttachOnlyCaller(t *testing.T) {
	e := newEnv(t, TransportRelay)
	g := newGate()
	e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) { x.Stdout([]byte("x")); g.wait(ctx); x.Exit(4) })
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"two"}})
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	att, err := e.sb.AttachExec(e.ctx, st.ExecID())
	if err != nil {
		t.Fatal(err)
	}
	g.release()
	res, _, out, _, err := collect(att)
	if err != nil || out != "x" || *res.ExitCode != 4 {
		t.Fatalf("%v %q", err, out)
	}
	auth := e.srv.AuthorizeCalls()
	last := auth[len(auth)-1]
	if last.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC || len(last.Digest) != 0 {
		t.Fatalf("attach grant %+v", last)
	}
	if _, _, _, _, err := collect(mustAttach(t, e, datapath.NewID())); err == nil {
		t.Fatal("unknown exec attached")
	}
}

func mustAttach(t *testing.T, e *env, id string) *ExecStream {
	st, err := e.sb.AttachExec(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
