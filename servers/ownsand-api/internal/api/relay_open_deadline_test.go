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

// TestRelayAdmissionStreamOpenWait checks the wait to open a stream on an
// agent relay connection whose outbound queue is full (nothing sends the
// queued frames), both while the call waits for room in the queue and while
// it waits for another open that holds the open order: a server-side deadline
// ends the call with deadline_exceeded and a cancellation with canceled. A
// connection that closed during the wait stays unavailable.
func TestRelayAdmissionStreamOpenWait(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: 5 * time.Second, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, _, sandboxID := runningSandbox(t, env)
	ctx := context.Background()

	// A connection without a writer: its outbound queue (1024 ordinary
	// frames) fills with stream open frames and no more room appears.
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

	expect := func(t *testing.T, name string) {
		t.Helper()
		t.Run(name+"/deadline", func(t *testing.T) {
			before := countRPCCode(env, readFileProcedure, connect.CodeDeadlineExceeded)
			start := time.Now()
			_, err := admissionCall(ctx, t, env, org, sandboxID, 200*time.Millisecond)
			took := time.Since(start)
			if testenv.Code(err) != connect.CodeDeadlineExceeded {
				t.Fatalf("deadline during the stream open: %v after %v, want deadline_exceeded", err, took)
			}
			if took > 3*time.Second {
				t.Fatalf("the call ended after %v, want about the 200 ms server deadline", took)
			}
			waitRPCCode(t, env, readFileProcedure, connect.CodeDeadlineExceeded, before)
		})
		t.Run(name+"/cancel", func(t *testing.T) {
			before := countRPCCode(env, readFileProcedure, connect.CodeCanceled)
			grant := authorizeReadFile(t, org, sandboxID)
			cctx, cancel := context.WithCancel(ctx)
			go func() {
				time.Sleep(150 * time.Millisecond)
				cancel()
			}()
			err := callReadFile(cctx, t, env, org, grant, 0)
			if testenv.Code(err) != connect.CodeCanceled {
				t.Fatalf("cancellation during the stream open: %v, want canceled", err)
			}
			waitRPCCode(t, env, readFileProcedure, connect.CodeCanceled, before)
		})
	}
	// The call itself waits for room in the full queue.
	expect(t, "queue full")

	// Another open holds the open order while it waits for room, so the call
	// waits for its turn.
	holderCtx, stopHolder := context.WithCancel(ctx)
	holderDone := make(chan error, 1)
	go func() {
		_, err := conn.Open(holderCtx, "holder", sandboxID)
		holderDone <- err
	}()
	time.Sleep(200 * time.Millisecond) // the holder takes the open order and blocks
	expect(t, "open order held")

	// A connection that closes while the call waits stays unavailable, and
	// the call does not report a context error.
	closing := authorizeReadFile(t, org, sandboxID)
	go func() {
		time.Sleep(200 * time.Millisecond)
		conn.Close(fmt.Errorf("the agent relay connection closed in the test"))
	}()
	start := time.Now()
	err := callReadFile(ctx, t, env, org, closing, time.Minute)
	if testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("connection closed during the stream open: %v after %v, want unavailable", err, time.Since(start))
	}
	stopHolder()
	<-holderDone
}
