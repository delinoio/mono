// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pionstun "github.com/pion/stun/v3"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/stun"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

const (
	meterRelay  = ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS
	meterR2     = ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE
	meterClassA = ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC
	meterClassB = ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC
)

// usageOf flushes the in-memory counts and returns the counted usage of the
// organization by meter.
func usageOf(t *testing.T, env *testenv.Env, orgID string) map[ownsandv1.BillingMeter]int64 {
	t.Helper()
	ctx := context.Background()
	if err := env.App.FlushUsage(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := env.DB.Pool.Query(ctx, "SELECT meter, sum(quantity)::bigint FROM usage_counters WHERE organization_id = $1 GROUP BY meter", orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[ownsandv1.BillingMeter]int64{}
	for rows.Next() {
		var meter int16
		var q int64
		if err := rows.Scan(&meter, &q); err != nil {
			t.Fatal(err)
		}
		out[ownsandv1.BillingMeter(meter)] = q
	}
	return out
}

func expectUsage(t *testing.T, env *testenv.Env, orgID string, want map[ownsandv1.BillingMeter]int64) {
	t.Helper()
	got := usageOf(t, env, orgID)
	for _, m := range []ownsandv1.BillingMeter{meterRelay, meterR2, meterClassA, meterClassB} {
		if got[m] != want[m] {
			t.Fatalf("usage of %s: %v = %d, want %d (all: %v)", orgID, m, got[m], want[m], got)
		}
	}
}

// TestRPCMetering checks that only authenticated RPCs with one organization
// that end with OK are counted, in their class, and that rejected calls,
// the agent channel and STUN produce no usage.
func TestRPCMetering(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	// CreateOrganization and AcceptInvitation are not billed; the two
	// invitations and the API key are Class A.
	org := env.NewOrg("alice")
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 3})

	owner, key := org.OwnerClient, env.Clients(org.APIKey)
	if _, err := owner.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := key.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 3, meterClassB: 2})

	// Rejected calls are not counted: authentication, authorization,
	// validation and not found.
	missingID, _ := uuidv7.NewString()
	rejected := []struct {
		name string
		call func() error
		code connect.Code
	}{
		{"unauthenticated", func() error {
			_, err := env.Clients("bad-token").Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID}))
			return err
		}, connect.CodeUnauthenticated},
		{"not a member", func() error {
			_, err := env.User("mallory").Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID}))
			return err
		}, connect.CodeNotFound},
		{"role not allowed", func() error {
			_, err := env.User(org.Member).Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
			return err
		}, connect.CodePermissionDenied},
		{"API key not accepted", func() error {
			_, err := key.Member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: org.ID}))
			return err
		}, connect.CodePermissionDenied},
		{"invalid argument", func() error {
			_, err := owner.Sandbox.CreateSandbox(ctx, connect.NewRequest(&ownsandv1.CreateSandboxRequest{OrganizationId: org.ID}))
			return err
		}, connect.CodeInvalidArgument},
		{"not found", func() error {
			_, err := owner.Sandbox.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: org.ID, SandboxId: missingID}))
			return err
		}, connect.CodeNotFound},
	}
	for _, r := range rejected {
		if err := r.call(); testenv.Code(err) != r.code {
			t.Fatalf("%s: %v", r.name, err)
		}
	}
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 3, meterClassB: 2})

	// CreateEnrollmentToken and Enroll are Class A; UpdateDeviceLimits too.
	// The device authentication and the agent channel are not billed.
	_, agent := setupAgent(t, env, org, nil)
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 6, meterClassB: 2})
	for seq := uint64(1); seq <= 3; seq++ {
		agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: seq}}})
		agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == seq })
	}
	agent.SendStateReport()
	agent.Close()
	testenv.Eventually(t, "channel closed", func() bool { return env.App.Service.Hub.Live(agent.Device.ID) == nil })

	// STUN binding requests are not billed.
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stunCtx, stopSTUN := context.WithCancel(ctx)
	defer stopSTUN()
	go func() { _ = stun.New(env.Logger, 1024, 4096).Serve(stunCtx, udp) }()
	conn, err := net.Dial("udp", udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	if _, err := conn.Write(req.Raw); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("stun response: %v", err)
	}
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 6, meterClassB: 2})

	// Usage written before a restart stays counted, and the new instance
	// keeps adding to it.
	env.Restart()
	if _, err := env.User(org.Owner).Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 6, meterClassB: 3})
}

// TestDeleteOrganizationKeepsUsage checks that the usage of a deleted
// organization, including the deletion call itself, stays counted.
func TestDeleteOrganizationKeepsUsage(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	if _, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	expectUsage(t, env, org.ID, map[ownsandv1.BillingMeter]int64{meterClassA: 4})
}

// countingTransport counts the response body bytes of every request.
type countingTransport struct {
	base http.RoundTripper
	n    atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = countingBody{ReadCloser: resp.Body, n: &c.n}
	return resp, nil
}

// TestRelayEgressMetering checks that the relay egress toward the caller is
// the exact response body bytes of Exec and ReadFile, framing included, and
// that a WriteFile request frame toward the agent is counted.
func TestRelayEgressMetering(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	// Random contents, so compression does not shrink them.
	content := make([]byte, 64<<10)
	_, _ = rand.Read(content)
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		switch {
		case f.GetExecRequest() != nil:
			serveExec(s, f)
		case f.GetFileReadRequest() != nil:
			s.Send(&ownsandv1.RequestAccepted{})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 0, Data: content}}})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: 64 << 10}}})
		case f.GetFileWriteChunk() != nil:
			w := f.GetFileWriteChunk()
			s.Send(&ownsandv1.WriteFileResponse{WriteId: w.GetWriteId(), Offset: w.GetOffset(), Length: int64(len(w.GetData()))})
		}
	})
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	if got := usageOf(t, env, org.ID)[meterRelay]; got != 0 {
		t.Fatalf("relay egress before any relay call = %d", got)
	}
	key := env.Clients(org.APIKey)
	counter := &countingTransport{base: env.Server.Client().Transport}
	httpClient := &http.Client{Transport: counter}

	// Exec.
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, key, req)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := env.ClientsWithHTTP(httpClient, org.APIKey, grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collectExec(t, stream); err != nil {
		t.Fatal(err)
	}
	// ReadFile.
	readID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	read := &ownsandv1.ReadFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, ReadId: readID, Path: "/tmp/f", BindingSalt: salt}
	rg := authorize(t, key, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, readID, relaybinding.ReadFileDigest(read))
	rs, err := env.ClientsWithHTTP(httpClient, org.APIKey, grantHeader(rg)).Relay.ReadFile(ctx, connect.NewRequest(read))
	if err != nil {
		t.Fatal(err)
	}
	for rs.Receive() {
	}
	if rs.Err() != nil {
		t.Fatal(rs.Err())
	}
	received := counter.n.Load()
	if received < 64<<10 {
		t.Fatalf("received %d bytes", received)
	}
	// The server counts a write when it returns, which can be just after
	// the client read the bytes.
	testenv.Eventually(t, "caller egress counted", func() bool { return usageOf(t, env, org.ID)[meterRelay] == received })

	// A relay call that does not match its grant writes an error but no
	// billable egress.
	other := newExec(org.ID, sandboxID)
	bad, err := env.ClientsWithHTTP(httpClient, org.APIKey, grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(other))
	if err == nil {
		for bad.Receive() {
		}
		err = bad.Err()
	}
	if testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("mismatched grant: %v", err)
	}
	if got := usageOf(t, env, org.ID)[meterRelay]; got != received {
		t.Fatalf("a rejected relay call added egress: %d != %d", got, received)
	}

	// WriteFile: the request frame toward the agent is egress; its small
	// response toward the caller is not.
	data := content[:32<<10]
	writeID, _ := uuidv7.NewString()
	write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: data}
	wg := authorize(t, key, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(write))
	if _, err := env.ClientsWith(org.APIKey, grantHeader(wg)).Relay.WriteFile(ctx, connect.NewRequest(write)); err != nil {
		t.Fatal(err)
	}
	frame := r.WaitFrame("write request", func(f *ownsandv1.DataFrame) bool { return f.GetFileWriteChunk().GetWriteId() == writeID })
	// The frame leaves the Droplet in one Connect message: its encoding plus
	// the 5-byte envelope prefix, less when the agent accepts compression.
	upper := int64(proto.Size(&ownsandv1.ConnectResponse{Frame: frame})) + 5 + 64
	egress := usageOf(t, env, org.ID)[meterRelay] - received
	if egress <= 0 || egress > upper {
		t.Fatalf("WriteFile egress = %d, want 1..%d", egress, upper)
	}
}
