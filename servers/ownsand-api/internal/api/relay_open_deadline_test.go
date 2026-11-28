// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// readFileProcedure is the procedure that the admission tests call and that
// the server's rpc log records carry.
const readFileProcedure = "/delino.ownsand.v1.RelayService/ReadFile"

// timeoutTransport authenticates a relay call with the grant header and sends
// the Connect timeout header, so the server enforces the deadline itself
// while the client keeps waiting for the server's answer. The Connect client
// would otherwise end the call locally at its own deadline and hide the code
// that the server returns.
type timeoutTransport struct {
	base    http.RoundTripper
	token   string
	grant   string
	timeout time.Duration
}

func (tt timeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tt.token)
	r.Header.Set(api.RelayGrantHeader, tt.grant)
	if tt.timeout > 0 {
		r.Header.Set("Connect-Timeout-Ms", strconv.FormatInt(tt.timeout.Milliseconds(), 10))
	}
	return tt.base.RoundTrip(r)
}

// admissionCall authorizes a ReadFile call and runs it with the server-side
// deadline timeout (none when zero). ctx is the client context, which has no
// deadline of its own. It returns the grant ID and the error of the call.
func admissionCall(ctx context.Context, t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, timeout time.Duration) (string, error) {
	t.Helper()
	grant := authorizeReadFile(t, org, sandboxID)
	return grant.id, callReadFile(ctx, t, env, org, grant, timeout)
}

type readGrant struct {
	id  string
	req *ownsandv1.ReadFileRequest
}

func authorizeReadFile(t *testing.T, org *testenv.Org, sandboxID string) readGrant {
	t.Helper()
	readID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	read := &ownsandv1.ReadFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, ReadId: readID, Path: "/etc/hostname", BindingSalt: salt}
	id := authorize(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, readID, relaybinding.ReadFileDigest(read))
	return readGrant{id: id, req: read}
}

func callReadFile(ctx context.Context, t *testing.T, env *testenv.Env, org *testenv.Org, g readGrant, timeout time.Duration) error {
	httpClient := &http.Client{Transport: timeoutTransport{base: env.Server.Client().Transport, token: testenv.UserToken(org.Owner), grant: g.id, timeout: timeout}}
	client := ownsandv1connect.NewRelayServiceClient(httpClient, env.Server.URL)
	stream, err := client.ReadFile(ctx, connect.NewRequest(g.req))
	return drain(t, stream, err)
}

// rpcCodes returns the codes of the server's rpc log records of procedure.
func rpcCodes(env *testenv.Env, procedure string) []string {
	var out []string
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		var rec struct {
			Msg       string `json:"msg"`
			Procedure string `json:"procedure"`
			Code      string `json:"code"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "rpc" && rec.Procedure == procedure {
			out = append(out, rec.Code)
		}
	}
	return out
}

// countRPCCode counts the rpc log records of procedure with code.
func countRPCCode(env *testenv.Env, procedure string, code connect.Code) int {
	n := 0
	for _, c := range rpcCodes(env, procedure) {
		if c == code.String() {
			n++
		}
	}
	return n
}

// waitRPCCode waits for a new rpc log record of procedure with code, beyond
// the before records that existed when the call started, since the server
// logs a rejected call after it answered.
func waitRPCCode(t *testing.T, env *testenv.Env, procedure string, code connect.Code, before int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countRPCCode(env, procedure, code) > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no new rpc log record of %s with code %s; codes: %v", procedure, code, rpcCodes(env, procedure))
}

// TestRelayAdmissionDeviceConnectionWait checks the wait for the agent relay
// connection: a server-side deadline that passes during the wait ends the
// call with deadline_exceeded, a cancellation ends it with canceled, and the
// expiry of the connection wait itself, or a device without any relay
// connection, keeps unavailable.
func TestRelayAdmissionDeviceConnectionWait(t *testing.T) {
	t.Parallel()
	const connWait = 700 * time.Millisecond
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: 5 * time.Second, GrantAck: time.Second, Signaling: 2 * time.Second, ConnWait: connWait}
	})
	org, _, sandboxID := runningSandbox(t, env)
	// The agent has no relay connection.

	t.Run("deadline", func(t *testing.T) {
		before := countRPCCode(env, readFileProcedure, connect.CodeDeadlineExceeded)
		start := time.Now()
		_, err := admissionCall(context.Background(), t, env, org, sandboxID, 150*time.Millisecond)
		took := time.Since(start)
		if testenv.Code(err) != connect.CodeDeadlineExceeded {
			t.Fatalf("deadline during the connection wait: %v after %v, want deadline_exceeded", err, took)
		}
		if took >= connWait {
			t.Fatalf("the call ended after %v, want about the 150 ms server deadline, before the %v connection wait", took, connWait)
		}
		waitRPCCode(t, env, readFileProcedure, connect.CodeDeadlineExceeded, before)
	})
	t.Run("cancel", func(t *testing.T) {
		before := countRPCCode(env, readFileProcedure, connect.CodeCanceled)
		grant := authorizeReadFile(t, org, sandboxID)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		err := callReadFile(ctx, t, env, org, grant, 0)
		if took := time.Since(start); took >= connWait {
			t.Fatalf("the call ended after %v, want about the 150 ms cancellation", took)
		}
		// The client sees its own cancellation; the server records the code
		// that it assigned.
		if testenv.Code(err) != connect.CodeCanceled {
			t.Fatalf("cancellation during the connection wait: %v, want canceled", err)
		}
		waitRPCCode(t, env, readFileProcedure, connect.CodeCanceled, before)
	})
	t.Run("connection wait expires", func(t *testing.T) {
		start := time.Now()
		// The server deadline is far beyond the connection wait.
		_, err := admissionCall(context.Background(), t, env, org, sandboxID, time.Minute)
		took := time.Since(start)
		if testenv.Code(err) != connect.CodeUnavailable {
			t.Fatalf("expired connection wait: %v after %v, want unavailable", err, took)
		}
		if took < connWait/2 || took > 5*time.Second {
			t.Fatalf("the call ended after %v, want about the %v connection wait", took, connWait)
		}
	})
	t.Run("no deadline", func(t *testing.T) {
		_, err := admissionCall(context.Background(), t, env, org, sandboxID, 0)
		if testenv.Code(err) != connect.CodeUnavailable {
			t.Fatalf("expired connection wait without a deadline: %v, want unavailable", err)
		}
	})
}

type relayAdmissionWait string

const (
	relayAdmissionQueue relayAdmissionWait = "outbound queue"
	relayAdmissionGate  relayAdmissionWait = "open gate"
)

// waitRelayAdmissionOpen observes the server goroutine of the one active
// ReadFile RPC. A direct Open by the fixture's holder has no DataPlane.open
// frame. The top frame identifies which select blocks the RPC.
func waitRelayAdmissionOpen(want relayAdmissionWait, within time.Duration) bool {
	const connFrame = "github.com/delinoio/mono/servers/ownsand-api/internal/relay.(*Conn)."
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, trace := range strings.Split(goroutineStacks(), "\n\n") {
			header, frames, _ := strings.Cut(trace, "\n")
			if !strings.Contains(header, "[select") ||
				!strings.Contains(frames, "(*DataPlane).open(") ||
				!strings.Contains(frames, connFrame+"Open(") {
				continue
			}
			if want == relayAdmissionGate && strings.HasPrefix(frames, connFrame+"Open(") ||
				want == relayAdmissionQueue && strings.HasPrefix(frames, connFrame+"enqueueFrame(") {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestRelayAdmissionStreamOpenWait checks the Connect status of a ReadFile
// waiting for queue capacity or for another open to release the gate.
func TestRelayAdmissionStreamOpenWait(t *testing.T) {
	// Not parallel: the test reads the stacks of every goroutine in the
	// process, so goroutines of a parallel test would change its result.
	const (
		waitWithin     = 2 * time.Second
		serverDeadline = 3 * time.Second
	)
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: 5 * time.Second, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, _, sandboxID := runningSandbox(t, env)
	ctx := t.Context()

	// The connection has no writer. Once full, the outbound queue stays full.
	probe := authorizeReadFile(t, org, sandboxID)
	g := env.App.Plane.Grants.Get(probe.id)
	if g == nil {
		t.Fatal("the probe grant is not admittable")
	}
	limits := relay.DefaultLimits()
	limits.MaxStreamsPerConn = 4096
	conn := relay.NewConn(ctx, g.DeviceID, g.ChannelGen, limits)
	t.Cleanup(func() { conn.Close(fmt.Errorf("test ended")) })
	if err := env.App.Plane.Relays.Register(conn, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1024; i++ {
		fillCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := conn.Open(fillCtx, fmt.Sprintf("filler-%d", i), sandboxID)
		cancel()
		if err != nil {
			t.Fatalf("fill the outbound queue, open %d: %v", i, err)
		}
	}
	fillerCount := conn.StreamCount()

	startCall := func(t *testing.T, timeout time.Duration) (context.CancelFunc, <-chan error) {
		t.Helper()
		grant := authorizeReadFile(t, org, sandboxID)
		clientCtx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			defer close(done)
			done <- callReadFile(clientCtx, t, env, org, grant, timeout)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(waitWithin):
				t.Error("ReadFile did not exit after cleanup canceled its context")
			}
		})
		return cancel, done
	}
	requireCode := func(t *testing.T, done <-chan error, want connect.Code) {
		t.Helper()
		select {
		case err := <-done:
			if testenv.Code(err) != want {
				t.Fatalf("ReadFile returned %v, want %s", err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("ReadFile did not return, want %s", want)
		}
	}
	requireWait := func(t *testing.T, want relayAdmissionWait) {
		t.Helper()
		if !waitRelayAdmissionOpen(want, waitWithin) {
			t.Fatalf("ReadFile did not reach the %s wait within %v", want, waitWithin)
		}
		if want == relayAdmissionGate {
			if got := conn.StreamCount(); got != fillerCount+1 {
				t.Fatalf("stream count while ReadFile waits at the open gate = %d, want %d with the holder active", got, fillerCount+1)
			}
		}
	}
	expect := func(t *testing.T, want relayAdmissionWait, activeStreams int) {
		t.Helper()
		checkStreams := func(t *testing.T) {
			t.Helper()
			if got := conn.StreamCount(); got != activeStreams {
				t.Fatalf("open streams after rejected admission = %d, want %d", got, activeStreams)
			}
		}
		t.Run("deadline", func(t *testing.T) {
			before := countRPCCode(env, readFileProcedure, connect.CodeDeadlineExceeded)
			start := time.Now()
			_, done := startCall(t, serverDeadline)
			requireWait(t, want)
			requireCode(t, done, connect.CodeDeadlineExceeded)
			if took := time.Since(start); took > serverDeadline+waitWithin {
				t.Fatalf("the call ended after %v, want about the %v server deadline", took, serverDeadline)
			}
			waitRPCCode(t, env, readFileProcedure, connect.CodeDeadlineExceeded, before)
			checkStreams(t)
		})
		t.Run("cancel", func(t *testing.T) {
			before := countRPCCode(env, readFileProcedure, connect.CodeCanceled)
			cancel, done := startCall(t, 0)
			requireWait(t, want)
			if want == relayAdmissionQueue {
				// With no holder, a queue wait must fail the gate observation
				// even though the RPC will return the same cancellation code.
				const controlWithin = 100 * time.Millisecond
				if waitRelayAdmissionOpen(relayAdmissionGate, controlWithin) {
					t.Fatal("the queue wait satisfied the open gate observation without a holder")
				}
				t.Logf("the open gate observation rejected the queue wait after %v", controlWithin)
			}
			cancel()
			requireCode(t, done, connect.CodeCanceled)
			waitRPCCode(t, env, readFileProcedure, connect.CodeCanceled, before)
			checkStreams(t)
		})
	}

	t.Run("queue full", func(t *testing.T) {
		expect(t, relayAdmissionQueue, fillerCount)
	})
	t.Run("open order held", func(t *testing.T) {
		holderCtx, stopHolder := context.WithCancel(t.Context())
		holderDone := make(chan struct{})
		t.Cleanup(func() {
			stopHolder()
			select {
			case <-holderDone:
			case <-time.After(waitWithin):
				t.Error("the open gate holder did not exit after cleanup canceled its context")
			}
		})
		go func() {
			defer close(holderDone)
			_, _ = conn.Open(holderCtx, "holder", sandboxID)
		}()

		// Open reserves its stream after acquiring the gate and before
		// enqueueing. The full queue keeps the holder from releasing the gate.
		deadline := time.Now().Add(waitWithin)
		for conn.StreamCount() != fillerCount+1 {
			if time.Now().After(deadline) {
				t.Fatalf("stream count while starting the open gate holder = %d, want %d within %v", conn.StreamCount(), fillerCount+1, waitWithin)
			}
			time.Sleep(5 * time.Millisecond)
		}
		expect(t, relayAdmissionGate, fillerCount+1)
		t.Run("close", func(t *testing.T) {
			// The client has no deadline, and the server deadline is longer
			// than the wait for the RPC to reach the gate and return.
			_, done := startCall(t, time.Minute)
			requireWait(t, relayAdmissionGate)
			conn.Close(fmt.Errorf("the agent relay connection closed in the test"))
			requireCode(t, done, connect.CodeUnavailable)
		})
	})
}
