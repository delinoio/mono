// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// rawPost sends one unary Connect request body as is and returns the HTTP
// status and the Connect error code of the response ("" for a success).
func rawPost(t *testing.T, env *testenv.Env, procedure, contentType, encoding, bearer string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, env.Server.URL+procedure, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := env.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", procedure, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("POST %s: status %d with a body that is no Connect error: %q", procedure, resp.StatusCode, data)
	}
	return resp.StatusCode, e.Code
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// enrollRequestOfSize returns an EnrollRequest that encodes to exactly size
// bytes, padded in its name field.
func enrollRequestOfSize(t *testing.T, size int) []byte {
	t.Helper()
	m := &ownsandv1.EnrollRequest{}
	m.Name = strings.Repeat("x", size)
	for n := proto.Size(m); n != size; n = proto.Size(m) {
		m.Name = strings.Repeat("x", len(m.Name)+size-n)
	}
	b, err := proto.Marshal(m)
	if err != nil || len(b) != size {
		t.Fatalf("marshal enroll request of %d bytes: %d bytes, %v", size, len(b), err)
	}
	return b
}

// TestRequestSizeLimit checks that a request message over api.MaxRequestBytes
// ends with resource_exhausted before the interceptor chain: no authentication
// failure, no rate limit token and no RPC log record. The limit holds for the
// binary and the JSON codec and for the inflated size of a compressed body.
func TestRequestSizeLimit(t *testing.T) {
	const (
		enroll      = ownsandv1connect.AgentServiceEnrollProcedure
		listDevices = ownsandv1connect.DeviceServiceListDevicesProcedure
		// A well-formed API key that does not exist: a request that reaches
		// authentication fails with unauthenticated and takes one token of
		// the failed-key bucket.
		badKey = "osk_BBBBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	limits := api.Limits{
		PerDevice: ratelimit.New(1000, 1000, nil),
		// One token: the first failed authentication empties the bucket.
		PerPrefix: ratelimit.New(0.0001, 1, nil),
		PerClient: ratelimit.New(1000, 1000, nil),
	}
	env := testenv.New(t, func(o *app.Options) { o.Limits = &limits })
	org := env.NewOrg("alice")
	over := api.MaxRequestBytes + 1
	logsBefore := len(env.Logs.String())

	t.Run("binary body over the limit", func(t *testing.T) {
		status, code := rawPost(t, env, enroll, "application/proto", "", "", enrollRequestOfSize(t, over))
		if status != http.StatusTooManyRequests || code != "resource_exhausted" {
			t.Fatalf("status %d code %q, want 429 resource_exhausted", status, code)
		}
	})
	t.Run("JSON body over the limit", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"name": strings.Repeat("x", over)})
		status, code := rawPost(t, env, enroll, "application/json", "", "", body)
		if status != http.StatusTooManyRequests || code != "resource_exhausted" {
			t.Fatalf("status %d code %q, want 429 resource_exhausted", status, code)
		}
	})
	t.Run("compressed body that inflates over the limit", func(t *testing.T) {
		packed := gzipBytes(t, enrollRequestOfSize(t, 8*api.MaxRequestBytes))
		if len(packed) > api.MaxRequestBytes/4 {
			t.Fatalf("the test body is not a decompression bomb: %d compressed bytes", len(packed))
		}
		status, code := rawPost(t, env, enroll, "application/proto", "gzip", "", packed)
		if status != http.StatusTooManyRequests || code != "resource_exhausted" {
			t.Fatalf("status %d code %q, want 429 resource_exhausted", status, code)
		}
	})
	t.Run("oversized request is rejected before authentication", func(t *testing.T) {
		status, code := rawPost(t, env, listDevices, "application/proto", "", badKey, enrollRequestOfSize(t, over))
		if status != http.StatusTooManyRequests || code != "resource_exhausted" {
			t.Fatalf("status %d code %q, want 429 resource_exhausted, not 401", status, code)
		}
	})
	t.Run("4 MiB labels map", func(t *testing.T) {
		req := createReq(org.ID)
		req.Labels = map[string]string{"big": strings.Repeat("x", 4<<20)}
		_, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(req))
		if testenv.Code(err) != connect.CodeResourceExhausted {
			t.Fatalf("CreateSandbox with 4 MiB labels: %v, want resource_exhausted", err)
		}
	})
	if logs := env.Logs.String()[logsBefore:]; strings.Contains(logs, `"msg":"rpc"`) {
		t.Fatalf("a rejected oversized request reached the interceptor chain and wrote an RPC log record:\n%s", logs)
	}
	t.Run("no rate limit token was taken", func(t *testing.T) {
		// The oversized request above carried the bad key. Its token is still
		// there: the first small request fails authentication, which takes
		// the token, and the next one is rate limited.
		small := enrollRequestOfSize(t, 16)
		if status, code := rawPost(t, env, listDevices, "application/proto", "", badKey, small); status != http.StatusUnauthorized || code != "unauthenticated" {
			t.Fatalf("first small request: status %d code %q, want 401 unauthenticated", status, code)
		}
		if _, code := rawPost(t, env, listDevices, "application/proto", "", badKey, small); code != "resource_exhausted" {
			t.Fatalf("second small request: code %q, want resource_exhausted from the failed-key bucket", code)
		}
	})
	t.Run("a message of exactly the limit is read", func(t *testing.T) {
		// The padded enroll request is invalid, which the handler reports
		// after it read the message; the size limit does not.
		status, code := rawPost(t, env, enroll, "application/proto", "", "", enrollRequestOfSize(t, api.MaxRequestBytes))
		if code == "resource_exhausted" {
			t.Fatalf("a message of exactly %d bytes was rejected: status %d", api.MaxRequestBytes, status)
		}
	})
}

// TestRequestSizeLimitAllowsMaxima checks that the largest legitimate request
// of each kind stays under the limit.
func TestRequestSizeLimitAllowsMaxima(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	relayConn := agent.ConnectRelay(serveRelay)
	relayConn.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	stop := keepAlive(relayConn)
	defer stop()
	owner := testenv.UserToken(org.Owner)

	writeFile := func(t *testing.T, opts ...connect.ClientOption) {
		t.Helper()
		writeID, _ := uuidv7.NewString()
		salt, _ := relaybinding.NewSalt()
		write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: make([]byte, datapath.MaxFileChunkSize)}
		grant := authorize(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(write))
		httpClient := &http.Client{Transport: bodyDelayTransport{base: env.Server.Client().Transport, token: owner, grant: grant}}
		client := ownsandv1connect.NewRelayServiceClient(httpClient, env.Server.URL, opts...)
		resp, err := client.WriteFile(ctx, connect.NewRequest(write))
		if err != nil {
			t.Fatalf("WriteFile of exactly 1 MiB: %v", err)
		}
		if resp.Msg.GetLength() != datapath.MaxFileChunkSize {
			t.Fatalf("written length = %d", resp.Msg.GetLength())
		}
	}
	t.Run("WriteFile of 1 MiB with the binary codec", func(t *testing.T) { writeFile(t) })
	t.Run("WriteFile of 1 MiB with the JSON codec", func(t *testing.T) { writeFile(t, connect.WithProtoJSON()) })

	t.Run("Exec with 256 KiB of fields", func(t *testing.T) {
		req := newExec(org.ID, sandboxID)
		req.Env = nil
		req.Command = []string{"echo", ""}
		req.Command[1] = strings.Repeat("x", validate.MaxExecFieldsSize-validate.ExecFieldsSize(req))
		for validate.ExecFieldsSize(req) > validate.MaxExecFieldsSize {
			req.Command[1] = req.Command[1][1:]
		}
		for validate.ExecFieldsSize(req) < validate.MaxExecFieldsSize {
			req.Command[1] += "x"
		}
		grant, err := authorizeExec(t, org.OwnerClient, req)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := env.ClientsWith(owner, grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(req))
		if err := drain(t, stream, err); err != nil {
			t.Fatalf("Exec with 256 KiB of fields: %v", err)
		}
	})

	t.Run("CreateSandbox with 32 maximal labels", func(t *testing.T) {
		req := createReq(org.ID)
		req.Labels = map[string]string{}
		for i := 0; i < validate.MaxLabels; i++ {
			req.Labels[fmt.Sprintf("%0*d", validate.MaxLabelKeySize, i)] = strings.Repeat("v", validate.MaxLabelValueSize)
		}
		if _, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req)); err != nil {
			t.Fatalf("CreateSandbox with 32 maximal labels: %v", err)
		}
	})

	t.Run("OpenSession with a 64 KiB SDP offer", func(t *testing.T) {
		offer := "v=0\n" + strings.Repeat("a", 64<<10-len("v=0\n"))
		resp, err := env.Clients(org.APIKey).DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: offer}))
		if err != nil || resp.Msg.GetSdpAnswer() == "" {
			t.Fatalf("OpenSession with a 64 KiB offer: %v %v", resp, err)
		}
	})

	t.Run("relay DataFrame of exactly MaxFrameSize", func(t *testing.T) {
		// The frame names a stream that the control plane does not know, so it
		// is dropped after its size check. A frame that the read limit rejected
		// would end the relay connection with resource_exhausted instead.
		f := &ownsandv1.DataFrame{StreamId: 1 << 40, Payload: &ownsandv1.DataFrame_ExecEvent{ExecEvent: &ownsandv1.ExecEvent{
			Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, datapath.MaxFrameSize/2)}}}}}
		f.SandboxId = "s"
		for datapath.EncodedSize(f) < datapath.MaxFrameSize {
			f.SandboxId += strings.Repeat("s", datapath.MaxFrameSize-datapath.EncodedSize(f))
		}
		for datapath.EncodedSize(f) > datapath.MaxFrameSize {
			f.SandboxId = f.SandboxId[1:]
		}
		if got := datapath.EncodedSize(f); got != datapath.MaxFrameSize {
			t.Fatalf("test frame size = %d, want %d", got, datapath.MaxFrameSize)
		}
		relayConn.SendFrame(f)
		select {
		case <-relayConn.Closed():
			t.Fatal("the relay connection ended after a frame of exactly MaxFrameSize")
		case <-time.After(500 * time.Millisecond):
		}
		req := newExec(org.ID, sandboxID)
		grant, err := authorizeExec(t, org.OwnerClient, req)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := env.ClientsWith(owner, grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(req))
		if err := drain(t, stream, err); err != nil {
			t.Fatalf("Exec after the maximal frame: %v", err)
		}
	})
}

// stateReportOf returns a full state report of count sandbox entries in a
// terminal state that the control plane does not know, so that applying the
// report changes no sandbox and sends no command.
func stateReportOf(t *testing.T, agent *testenv.FakeAgent, sequence uint64, count int) *ownsandv1.StateReport {
	t.Helper()
	report := &ownsandv1.StateReport{ReportSequence: sequence, StateEpoch: agent.Device.Epoch}
	for i := 0; i < count; i++ {
		id, err := uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
		report.Sandboxes = append(report.Sandboxes, &ownsandv1.SandboxStateEntry{
			SandboxId: id, State: ownsandv1.SandboxState_SANDBOX_STATE_DELETED, EndReason: ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER,
			Revision: &ownsandv1.SandboxRevision{Epoch: agent.Device.Epoch, Counter: 1},
		})
	}
	return report
}

// TestAgentChannelMessageLimit checks that the limit applies to each message
// of the agent channel stream: many messages that are each under the limit
// pass, a state report of 5,000 sandboxes passes, and one message over the
// limit ends the stream with resource_exhausted.
func TestAgentChannelMessageLimit(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)

	const reports = 6
	var total int
	for i := 0; i < reports; i++ {
		seq := uint64(1000 + i)
		report := stateReportOf(t, agent, seq, 5000)
		total += proto.Size(report)
		agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: report}})
		agent.WaitFor(fmt.Sprintf("ack of state report %d", seq), func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetStateAck().GetStateReport().GetReportSequence() == seq
		})
	}
	if total <= api.MaxRequestBytes {
		t.Fatalf("the reports total %d bytes; they must exceed the per-message limit %d to show it is per message", total, api.MaxRequestBytes)
	}
	select {
	case <-agent.Closed():
		t.Fatalf("the channel ended after in-limit messages: %v", agent.CloseErr())
	default:
	}

	// One message over the limit: the stream ends, and the end carries the code.
	huge := &ownsandv1.StateReport{ReportSequence: 9999, StateEpoch: strings.Repeat("e", api.MaxRequestBytes+1)}
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: huge}})
	select {
	case <-agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel did not end after a message over the limit")
	}
	if code := testenv.Code(agent.CloseErr()); code != connect.CodeResourceExhausted {
		t.Fatalf("the channel ended with %v, want resource_exhausted", agent.CloseErr())
	}
}
