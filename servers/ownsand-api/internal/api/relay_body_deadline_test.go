// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// bodyDelayTransport delays the request body of the relay calls that carry
// the grant header, like a caller on a slow link: the headers go out at once
// and the first byte of the body follows after delay. A call that ends first
// unblocks the delayed read.
type bodyDelayTransport struct {
	base  http.RoundTripper
	delay time.Duration
	token string
	grant string
}

func (b bodyDelayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set(api.RelayGrantHeader, b.grant)
	if r.Body != nil {
		r.Body = &delayedBody{ReadCloser: r.Body, delay: b.delay, done: make(chan struct{})}
	}
	return b.base.RoundTrip(r)
}

type delayedBody struct {
	io.ReadCloser
	delay time.Duration
	once  sync.Once
	close sync.Once
	done  chan struct{}
}

func (d *delayedBody) Read(p []byte) (int, error) {
	var closed bool
	d.once.Do(func() {
		select {
		case <-time.After(d.delay):
		case <-d.done:
			closed = true
		}
	})
	if closed {
		return 0, io.ErrClosedPipe
	}
	return d.ReadCloser.Read(p)
}

func (d *delayedBody) Close() error {
	d.close.Do(func() { close(d.done) })
	return d.ReadCloser.Close()
}

// relayCase is one relay method with a way to authorize and call it.
type relayCase struct {
	name string
	// call authorizes the operation and runs the RPC through a client whose
	// request body arrives after delay. It returns the grant ID and the RPC
	// result: the error of the call, or of the response stream.
	call func(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, delay time.Duration) (grantID string, err error)
}

func delayedRelay(env *testenv.Env, org *testenv.Org, grant string, delay time.Duration) ownsandv1connect.RelayServiceClient {
	httpClient := &http.Client{Transport: bodyDelayTransport{base: env.Server.Client().Transport, delay: delay, token: testenv.UserToken(org.Owner), grant: grant}}
	return ownsandv1connect.NewRelayServiceClient(httpClient, env.Server.URL)
}

func drain[T any](t *testing.T, stream *connect.ServerStreamForClient[T], err error) error {
	t.Helper()
	if err != nil {
		return err
	}
	for stream.Receive() {
	}
	return stream.Err()
}

func relayCases() []relayCase {
	return []relayCase{
		{"Exec", func(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, delay time.Duration) (string, error) {
			req := newExec(org.ID, sandboxID)
			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := delayedRelay(env, org, grant.GetGrantId(), delay).Exec(context.Background(), connect.NewRequest(req))
			return grant.GetGrantId(), drain(t, stream, err)
		}},
		{"AttachExec", func(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, delay time.Duration) (string, error) {
			req := newExec(org.ID, sandboxID)
			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := delayedRelay(env, org, grant.GetGrantId(), delay).AttachExec(context.Background(),
				connect.NewRequest(&ownsandv1.AttachExecRequest{OrganizationId: org.ID, SandboxId: sandboxID, ExecId: req.GetExecId()}))
			return grant.GetGrantId(), drain(t, stream, err)
		}},
		{"ReadFile", func(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, delay time.Duration) (string, error) {
			readID, _ := uuidv7.NewString()
			salt, _ := relaybinding.NewSalt()
			read := &ownsandv1.ReadFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, ReadId: readID, Path: "/etc/hostname", BindingSalt: salt}
			grant := authorize(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, readID, relaybinding.ReadFileDigest(read))
			stream, err := delayedRelay(env, org, grant, delay).ReadFile(context.Background(), connect.NewRequest(read))
			return grant, drain(t, stream, err)
		}},
		{"WriteFile", func(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, delay time.Duration) (string, error) {
			writeID, _ := uuidv7.NewString()
			salt, _ := relaybinding.NewSalt()
			write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: []byte("chunk")}
			grant := authorize(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(write))
			_, err := delayedRelay(env, org, grant, delay).WriteFile(context.Background(), connect.NewRequest(write))
			return grant, err
		}},
	}
}

// serveRelay answers every request like an agent that completes it at once.
func serveRelay(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
	switch {
	case f.GetExecRequest() != nil:
		serveExec(s, f)
	case f.GetExecAttachRequest() != nil:
		s.Send(&ownsandv1.RequestAccepted{})
		s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}}})
	case f.GetFileReadRequest() != nil:
		s.Send(&ownsandv1.RequestAccepted{})
		s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: 0}}})
	case f.GetFileWriteChunk() != nil:
		w := f.GetFileWriteChunk()
		s.Send(&ownsandv1.WriteFileResponse{WriteId: w.GetWriteId(), Offset: w.GetOffset(), Length: int64(len(w.GetData()))})
	}
}

// streamOf returns the stream_id that the control plane allocated for grant.
func streamOf(r *testenv.FakeRelay, grant string) uint64 {
	return r.WaitFrame("stream open of the grant", func(f *ownsandv1.DataFrame) bool {
		return f.GetRelayStreamOpen().GetGrantId() == grant
	}).GetStreamId()
}

// TestRelayBodyDeadline checks that the request deadline counts from the
// stream allocation at the request gate and covers the request body upload:
// a body that arrives after the deadline ends the call with deadline_exceeded,
// and the agent sees a stream open and a stream close but no request frame.
func TestRelayBodyDeadline(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: 600 * time.Millisecond, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(serveRelay)
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	stop := keepAlive(r)
	defer stop()

	for _, tc := range relayCases() {
		t.Run(tc.name+"/stalled upload", func(t *testing.T) {
			start := time.Now()
			grant, err := tc.call(t, env, org, sandboxID, 5*time.Second)
			took := time.Since(start)
			if testenv.Code(err) != connect.CodeDeadlineExceeded {
				t.Fatalf("stalled upload: %v after %v, want deadline_exceeded", err, took)
			}
			if took > 3*time.Second {
				t.Fatalf("stalled upload ended after %v, want about the 600 ms request deadline", took)
			}
			id := streamOf(r, grant)
			r.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == id && f.GetRelayStreamClose() != nil })
			for _, f := range r.Frames() {
				if f.GetStreamId() == id && datapath.IsRequestKind(f.GetKind()) {
					t.Fatalf("the agent received a request frame %v of the stream %d", f.GetKind(), id)
				}
			}
			// One record must carry the grant and the code together: the
			// earlier cases of this test leave deadline_exceeded records of
			// their own grants in the same log.
			waitRelayRPC(t, env, grant, connect.CodeDeadlineExceeded)
		})
		t.Run(tc.name+"/slow upload within the budget", func(t *testing.T) {
			grant, err := tc.call(t, env, org, sandboxID, 200*time.Millisecond)
			if tc.name == "AttachExec" {
				// The fake agent answers an attach with an end of stream.
				if err != nil && testenv.Code(err) != connect.CodeNotFound {
					t.Fatalf("slow upload: %v", err)
				}
			} else if err != nil {
				t.Fatalf("slow upload: %v", err)
			}
			id := streamOf(r, grant)
			if got := countRequestFrames(r, id); got != 1 {
				t.Fatalf("request frames of stream %d = %d, want 1", id, got)
			}
		})
	}
}

// relayRPCRecord is the part of a relay_rpc log record that the tests match.
type relayRPCRecord struct {
	Msg       string `json:"msg"`
	GrantID   string `json:"grant_id"`
	Code      string `json:"code"`
	Procedure string `json:"procedure"`
}

// relayRPCRecords parses every relay_rpc record of the server log, one record
// per log line.
func relayRPCRecords(env *testenv.Env) []relayRPCRecord {
	var out []relayRPCRecord
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		var rec relayRPCRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Msg != "relay_rpc" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// relayRPCLines returns the relay_rpc log lines of the grant, for a failure
// message.
func relayRPCLines(env *testenv.Env, grant string) []string {
	var out []string
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		var rec relayRPCRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "relay_rpc" && rec.GrantID == grant {
			out = append(out, line)
		}
	}
	return out
}

// waitRelayRPC waits, within 10 s, for one relay_rpc log record that carries
// both grant_id grant and the code. It fails with the relay_rpc records of the
// grant when no such record appears.
func waitRelayRPC(t *testing.T, env *testenv.Env, grant string, code connect.Code) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, rec := range relayRPCRecords(env) {
			if rec.GrantID == grant && rec.Code == code.String() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for one relay_rpc log record with grant_id %s and code %s; relay_rpc records of the grant:\n%s",
				grant, code, strings.Join(relayRPCLines(env, grant), "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countRequestFrames(r *testenv.FakeRelay, id uint64) int {
	n := 0
	for _, f := range r.Frames() {
		if f.GetStreamId() == id && datapath.IsRequestKind(f.GetKind()) {
			n++
		}
	}
	return n
}

// TestRelayBudgetCoversUpload checks that WriteFile counts the upload against
// the same budget as the agent's result: a call whose upload takes part of the
// budget and whose agent never answers ends at the original absolute
// deadline, not one full deadline after the upload.
func TestRelayBudgetCoversUpload(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: time.Second, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {}) // never answers
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	stop := keepAlive(r)
	defer stop()

	cases := relayCases()
	write := cases[3]
	start := time.Now()
	grant, err := write.call(t, env, org, sandboxID, 600*time.Millisecond)
	took := time.Since(start)
	if testenv.Code(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("write: %v after %v, want deadline_exceeded", err, took)
	}
	// The upload took 600 ms of the 1 s budget. A deadline that restarted
	// after the upload would end the call after about 1.6 s.
	if took < 900*time.Millisecond || took > 1400*time.Millisecond {
		t.Fatalf("write ended after %v, want about the 1 s budget counted from the stream allocation", took)
	}
	id := streamOf(r, grant)
	r.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == id && f.GetRelayStreamClose() != nil })

	// The call must have failed because the agent never answered, not because
	// the relay gave up before it forwarded the upload: the agent has to have
	// received the write chunk, and before the deadline closed the stream.
	chunkAt, closeAt := -1, -1
	for i, f := range r.Frames() {
		if f.GetStreamId() != id {
			continue
		}
		if f.GetFileWriteChunk() != nil && chunkAt < 0 {
			chunkAt = i
			chunk := f.GetFileWriteChunk()
			if chunk.GetWriteId() == "" || chunk.GetOffset() != 0 || string(chunk.GetData()) != "chunk" {
				t.Fatalf("the forwarded write chunk is %v, want the uploaded data %q at offset 0", chunk, "chunk")
			}
		}
		if f.GetRelayStreamClose() != nil && closeAt < 0 {
			closeAt = i
		}
	}
	if chunkAt < 0 {
		t.Fatalf("the agent never received the file write chunk of stream %d", id)
	}
	if closeAt < 0 || chunkAt > closeAt {
		t.Fatalf("the file write chunk (frame %d) did not arrive before the stream close (frame %d)", chunkAt, closeAt)
	}
	if n := countRequestFrames(r, id); n != 1 {
		t.Fatalf("the agent received %d request frames for stream %d, want exactly the write chunk", n, id)
	}
}

// TestRelayGateRejectsBeforeBody checks that a caller with an invalid grant is
// rejected, and the rejection logged, before the request body arrives, and
// that no stream is allocated for it.
func TestRelayGateRejectsBeforeBody(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(serveRelay)
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

	req := newExec(org.ID, sandboxID)
	start := time.Now()
	// The body would arrive after 30 s; the gate rejects the unknown grant
	// from the headers alone.
	stream, err := delayedRelay(env, org, "01900000-0000-7000-8000-000000000000", 30*time.Second).Exec(context.Background(), connect.NewRequest(req))
	err = drain(t, stream, err)
	if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetSessionEndReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("unknown grant: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the rejection waited %v for the request body", took)
	}
	for _, f := range r.Frames() {
		if f.GetRelayStreamOpen() != nil {
			t.Fatalf("a stream open frame was sent for a rejected call: %v", f)
		}
	}
	if !strings.Contains(env.Logs.String(), `"msg":"rpc"`) || !strings.Contains(env.Logs.String(), ownsandv1connect.RelayServiceExecProcedure) {
		t.Fatal("missing RPC log record of the rejected call")
	}
}

// TestRelayGateGrantMismatch checks that a request whose body does not match
// the grant is rejected after decoding and that its stream, opened by the gate,
// is closed.
func TestRelayGateGrantMismatch(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(serveRelay)
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	other := newExec(org.ID, sandboxID) // another exec_id than the grant's
	stream, err := delayedRelay(env, org, grant.GetGrantId(), 0).Exec(context.Background(), connect.NewRequest(other))
	if err = drain(t, stream, err); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("mismatch: %v", err)
	}
	id := streamOf(r, grant.GetGrantId())
	r.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == id && f.GetRelayStreamClose() != nil })
	if n := countRequestFrames(r, id); n != 0 {
		t.Fatalf("the agent received %d request frames of a mismatching call", n)
	}
}
