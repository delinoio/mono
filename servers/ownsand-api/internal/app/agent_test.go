// SPDX-License-Identifier: FSL-1.1-ALv2

package app

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/nonce"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

func code(err error) connect.Code {
	if err == nil {
		return 0
	}
	return connect.CodeOf(err)
}

func (h *harness) device(org, id string) store.Device {
	h.t.Helper()
	d, err := h.store.GetDevice(context.Background(), org, id)
	must(h.t, err)
	return d
}

func (h *harness) setLimits(f fixture, deviceID string, l *ownsandv1.DeviceLimits) {
	h.t.Helper()
	_, err := h.clients(userToken(f.ownerSub)).device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: f.org, DeviceId: deviceID, Limits: l}))
	must(h.t, err)
}

func TestAgentAuthenticateAndRemoval(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)

	// Replay of a consumed nonce.
	_, n, err := a.authenticateWith(nonce.LabelAuthenticate)
	must(t, err)
	_, err = a.client.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{DeviceId: a.deviceID, Nonce: n, Signature: ed25519.Sign(a.priv, nonce.SignedMessage(nonce.LabelAuthenticate, n))}))
	if code(err) != connect.CodeUnauthenticated {
		t.Fatalf("replay: %v", err)
	}
	// A signature with the other purpose label.
	if _, _, err := a.authenticateWith(nonce.LabelConfirmRemoval); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("wrong label: %v", err)
	}
	// ConfirmRemoval of an enrolled device.
	ch, _ := a.client.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: a.deviceID}))
	_, err = a.client.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{DeviceId: a.deviceID, Nonce: ch.Msg.Nonce, Signature: ed25519.Sign(a.priv, nonce.SignedMessage(nonce.LabelConfirmRemoval, ch.Msg.Nonce))}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("confirm enrolled: %v", err)
	}
	// Unknown device.
	unknown := &fakeAgent{t: t, h: h, deviceID: datapath.NewID(), priv: a.priv, client: a.client}
	if _, _, err := unknown.authenticateWith(nonce.LabelAuthenticate); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown device: %v", err)
	}

	// The agent channel works with the credential.
	a.connect(nil)
	if d := h.device(f.org, a.deviceID); d.State != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state %v", d.State)
	}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: 7}}})
	a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 7 })

	// Removal sends the clean-up command and closes the channel.
	_, err = h.clients(userToken(f.ownerSub)).device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: f.org, DeviceId: a.deviceID}))
	must(t, err)
	if a.command().GetCleanUp() == nil {
		t.Fatal("no clean-up command")
	}
	if !a.waitClosed(5 * time.Second) {
		t.Fatal("channel of a removed device stayed open")
	}
	resp, _, err := a.authenticateWith(nonce.LabelAuthenticate)
	if err != nil || resp.GetRemovalNotice() == nil {
		t.Fatalf("removal notice: %v %v", resp, err)
	}
	ch, _ = a.client.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: a.deviceID}))
	_, err = a.client.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{DeviceId: a.deviceID, Nonce: ch.Msg.Nonce, Signature: ed25519.Sign(a.priv, nonce.SignedMessage(nonce.LabelConfirmRemoval, ch.Msg.Nonce))}))
	must(t, err)
	if _, _, err := a.authenticateWith(nonce.LabelAuthenticate); code(err) != connect.CodeNotFound {
		t.Fatalf("after confirmation: %v", err)
	}

	// A device removed by organization deletion also gets the notice.
	b := h.enrollAgent(f.org, f.ownerSub)
	_, err = h.clients(userToken(f.ownerSub)).org.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org}))
	must(t, err)
	resp, _, err = b.authenticateWith(nonce.LabelAuthenticate)
	if err != nil || resp.GetRemovalNotice() == nil {
		t.Fatalf("removal notice after organization deletion: %v %v", resp, err)
	}
}

func TestChannelAccessAndGenerations(t *testing.T) {
	h := newHarness(t, harnessOptions{app: func(a *App) { a.Agents.HeartbeatTimeout = time.Second }})
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	// A user credential cannot open the agent channel.
	stream := ownsandv1connect.NewAgentServiceClient(h.client, h.url).Channel(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+userToken(f.ownerSub))
	_ = stream.Send(&ownsandv1.AgentEnvelope{})
	if _, err := stream.Receive(); code(err) != connect.CodePermissionDenied {
		t.Fatalf("user on Channel: %v", err)
	}

	a.connect(nil)
	first := a
	// A second channel replaces the first; the old channel's close does not
	// mark the device offline.
	second := &fakeAgent{t: t, h: h, deviceID: a.deviceID, orgID: a.orgID, priv: a.priv, epoch: a.epoch, caps: a.caps, client: a.client}
	_ = first
	second.connect(nil)
	time.Sleep(200 * time.Millisecond)
	if d := h.device(f.org, a.deviceID); d.State != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("replaced channel changed the state: %v", d.State)
	}
	// Without heartbeats, the server closes the channel and marks the device offline.
	if !second.waitClosed(5 * time.Second) {
		t.Fatal("heartbeat timeout did not close the channel")
	}
	time.Sleep(200 * time.Millisecond)
	if d := h.device(f.org, a.deviceID); d.State != ownsandv1.DeviceState_DEVICE_STATE_OFFLINE {
		t.Fatalf("after heartbeat timeout: %v", d.State)
	}
}

func createReq(org string, mut func(*ownsandv1.CreateSandboxRequest)) *connect.Request[ownsandv1.CreateSandboxRequest] {
	r := &ownsandv1.CreateSandboxRequest{
		OrganizationId: org, IdempotencyKey: datapath.NewID(),
		Source:    &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
		Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10},
		IdleTtl:   durationpb.New(time.Hour), MaxTtl: durationpb.New(2 * time.Hour),
	}
	if mut != nil {
		mut(r)
	}
	return connect.NewRequest(r)
}

func sandboxClient(h *harness, cred string) ownsandv1connect.SandboxServiceClient {
	return ownsandv1connect.NewSandboxServiceClient(h.client, h.url, bearer(cred))
}

// TestCreateAfterLimitsChange checks that the agent gets the new limits
// before a create that the scheduler placed with them: the agent rejects a
// create above the limits of its last configuration.
func TestCreateAfterLimitsChange(t *testing.T) {
	h := newHarness(t)
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	if cfg := a.connect(nil); cfg.GetLimits().GetVcpuCount() != 0 {
		t.Fatalf("initial limits: %v", cfg.GetLimits())
	}
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 50})
	if _, err := sandboxClient(h, f.apiKey).CreateSandbox(context.Background(), createReq(f.org, nil)); err != nil {
		t.Fatal(err)
	}
	env := a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration() != nil || e.GetCommand() != nil })
	if env.GetConfiguration().GetLimits().GetVcpuCount() != 4 {
		t.Fatalf("the create command arrived before the new limits: %v", env)
	}
	if a.command().GetCreateSandbox() == nil {
		t.Fatal("no create command")
	}
}

func TestSandboxLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	c := sandboxClient(h, f.apiKey)

	// Zero limits: no eligible device, and an offline device is unavailable.
	if _, err := c.CreateSandbox(ctx, createReq(f.org, nil)); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("zero limits: %v", err)
	}
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 50})
	if _, err := c.CreateSandbox(ctx, createReq(f.org, nil)); code(err) != connect.CodeUnavailable {
		t.Fatalf("offline device: %v", err)
	}
	cfg := a.connect(nil)
	if cfg.GetLimits().GetVcpuCount() != 4 {
		t.Fatalf("configuration limits: %v", cfg.GetLimits())
	}

	req := createReq(f.org, func(r *ownsandv1.CreateSandboxRequest) { r.Labels = map[string]string{"team": "a"} })
	created, err := c.CreateSandbox(ctx, req)
	must(t, err)
	sb := created.Msg.Sandbox
	if sb.State != ownsandv1.SandboxState_SANDBOX_STATE_CREATING || sb.DeviceId != a.deviceID {
		t.Fatalf("created: %v", sb)
	}
	cmd := a.command().GetCreateSandbox()
	if cmd.GetSandboxId() != sb.SandboxId || cmd.GetLimits().GetVcpuCount() != 4 || cmd.GetMaxDeadline() == nil {
		t.Fatalf("create command: %v", cmd)
	}
	// Idempotent retry, and a reuse with other parameters.
	again, err := c.CreateSandbox(ctx, req)
	if err != nil || again.Msg.Sandbox.SandboxId != sb.SandboxId {
		t.Fatalf("idempotent retry: %v", err)
	}
	other := createReq(f.org, func(r *ownsandv1.CreateSandboxRequest) {
		r.IdempotencyKey = req.Msg.IdempotencyKey
		r.Resources.VcpuCount = 1
	})
	if _, err := c.CreateSandbox(ctx, other); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("idempotency mismatch: %v", err)
	}

	a.report(sb.SandboxId, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	got, err := c.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: f.org, SandboxId: sb.SandboxId}))
	if err != nil || got.Msg.Sandbox.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || got.Msg.Sandbox.Labels["team"] != "a" {
		t.Fatalf("running: %v %v", got, err)
	}
	// Capacity: a second sandbox does not fit (4 vCPU limit, 2 used, request 3).
	if _, err := c.CreateSandbox(ctx, createReq(f.org, func(r *ownsandv1.CreateSandboxRequest) { r.Resources.VcpuCount = 3 })); code(err) != connect.CodeUnavailable {
		t.Fatalf("capacity: %v", err)
	}

	// Pause: the agent answers with the paused sandbox.
	pauseDone := make(chan error, 1)
	go func() {
		resp, err := c.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: f.org, SandboxId: sb.SandboxId}))
		if err == nil && resp.Msg.Sandbox.State != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			err = connect.NewError(connect.CodeInternal, nil)
		}
		pauseDone <- err
	}()
	pause := a.command()
	if pause.GetPauseSandbox().GetSandboxId() != sb.SandboxId {
		t.Fatalf("pause command: %v", pause)
	}
	a.result(pause, "", &ownsandv1.SandboxReport{SandboxId: sb.SandboxId, State: ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, Revision: a.nextRevision()})
	if err := <-pauseDone; err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := c.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: f.org, SandboxId: sb.SandboxId})); err != nil {
		t.Fatalf("pause of a paused sandbox: %v", err)
	}

	// Delete waits for the agent result.
	delDone := make(chan *ownsandv1.Sandbox, 1)
	go func() {
		resp, err := c.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: f.org, SandboxId: sb.SandboxId}))
		if err != nil {
			delDone <- nil
			return
		}
		delDone <- resp.Msg.Sandbox
	}()
	del := a.command().GetDeleteSandbox()
	if del.GetSandboxId() != sb.SandboxId || del.GetReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("delete command: %v", del)
	}
	a.report(sb.SandboxId, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
	ended := <-delDone
	if ended == nil || ended.State != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || ended.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("deleted: %v", ended)
	}
	// Ended sandboxes are hidden unless asked for.
	list, _ := c.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: f.org}))
	all, _ := c.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: f.org, States: []ownsandv1.SandboxState{ownsandv1.SandboxState_SANDBOX_STATE_DELETED}}))
	if len(list.Msg.Sandboxes) != 0 || len(all.Msg.Sandboxes) != 1 {
		t.Fatalf("list: %d hidden filter, %d deleted filter", len(list.Msg.Sandboxes), len(all.Msg.Sandboxes))
	}
	var audits []int16
	rows, _ := h.store.Pool.Query(ctx, `SELECT action FROM audit_events WHERE target_id = $1 ORDER BY created_at`, sb.SandboxId)
	for rows.Next() {
		var x int16
		_ = rows.Scan(&x)
		audits = append(audits, x)
	}
	rows.Close()
	if len(audits) != 2 || audits[0] != int16(ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_CREATED) || audits[1] != int16(ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_DELETED) {
		t.Fatalf("audit: %v", audits)
	}

	// A create that the agent rejects ends as CREATE_FAILED.
	rejected, err := c.CreateSandbox(ctx, createReq(f.org, nil))
	must(t, err)
	a.result(a.command(), "resource_exhausted", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := c.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: f.org, SandboxId: rejected.Msg.Sandbox.SandboxId}))
		if got.Msg.Sandbox.State == ownsandv1.SandboxState_SANDBOX_STATE_FAILED && got.Msg.Sandbox.EndReason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rejected create: %v", got.Msg.Sandbox)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOfflineDeleteAndReconciliation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 100})
	a.connect(nil)
	c := sandboxClient(h, userToken(f.memberSub))
	var ids []string
	for i := 0; i < 3; i++ {
		resp, err := c.CreateSandbox(ctx, createReq(f.org, nil))
		must(t, err)
		ids = append(ids, resp.Msg.Sandbox.SandboxId)
		a.command()
		a.report(resp.Msg.Sandbox.SandboxId, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	}
	a.disconnect()
	time.Sleep(300 * time.Millisecond)
	get := func(id string) *ownsandv1.Sandbox {
		resp, err := c.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: f.org, SandboxId: id}))
		must(t, err)
		return resp.Msg.Sandbox
	}
	if sb := get(ids[0]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE || sb.LastKnownState != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("offline: %v %v", sb.State, sb.LastKnownState)
	}
	// Pause needs the device; delete records a pending delete.
	if _, err := c.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: f.org, SandboxId: ids[0]})); code(err) != connect.CodeUnavailable {
		t.Fatalf("pause offline: %v", err)
	}
	resp, err := c.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: f.org, SandboxId: ids[0]}))
	if err != nil || resp.Msg.Sandbox.State != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE || resp.Msg.Sandbox.LastKnownState != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("offline delete: %v %v", resp, err)
	}
	if _, err := c.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: f.org, SandboxId: ids[1]})); err != nil {
		t.Fatal(err)
	}

	// Reconnect: ids[0] is still running (same revision); ids[1] is gone;
	// ids[2] is not listed and was not being deleted.
	a.revision = 0
	report := &ownsandv1.StateReport{ReportId: 2, Sandboxes: []*ownsandv1.SandboxReport{
		{SandboxId: ids[0], State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, Revision: &ownsandv1.SandboxRevision{Epoch: a.epoch, Counter: 1}},
	}}
	a.connect(report)
	del := a.command().GetDeleteSandbox()
	if del.GetSandboxId() != ids[0] {
		t.Fatalf("delete after reconnection: %v", del)
	}
	if sb := get(ids[0]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("pending delete online: %v", sb.State)
	}
	if sb := get(ids[1]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("unlisted pending delete: %v %v", sb.State, sb.EndReason)
	}
	if sb := get(ids[2]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || sb.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("unlisted sandbox: %v %v", sb.State, sb.EndReason)
	}
	// Terminal states are absorbing: a later live report gets a delete command.
	a.revision = 5
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: &ownsandv1.SandboxReport{
		SandboxId: ids[2], State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, Revision: a.nextRevision(),
	}}}})
	if a.command().GetDeleteSandbox().GetSandboxId() != ids[2] {
		t.Fatal("no delete for a live VM of a failed sandbox")
	}
	if sb := get(ids[2]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		t.Fatalf("absorbing state changed: %v", sb.State)
	}
	// A stale revision is acknowledged without being applied.
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: &ownsandv1.SandboxReport{
		SandboxId: ids[0], State: ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, Revision: &ownsandv1.SandboxRevision{Epoch: a.epoch, Counter: 0},
	}}}})
	a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetSandbox().GetSandboxId() == ids[0] })
	if sb := get(ids[0]); sb.State != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("stale update applied: %v", sb.State)
	}
}

func TestConcurrentReservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10})
	a.connect(nil)
	c := sandboxClient(h, f.apiKey)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.CreateSandbox(ctx, createReq(f.org, nil))
		}(i)
	}
	wg.Wait()
	ok, unavailable := 0, 0
	for _, err := range errs {
		switch code(err) {
		case 0:
			ok++
		case connect.CodeUnavailable:
			unavailable++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || unavailable != 7 {
		t.Fatalf("ok=%d unavailable=%d", ok, unavailable)
	}
}

func TestSnapshotsTemplatesAndExport(t *testing.T) {
	var mu sync.Mutex
	puts := 0
	r2srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
			return
		}
		puts++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer r2srv.Close()
	h := newHarness(t, harnessOptions{cfg: func(c *config.Config) {
		c.Extra.R2 = config.R2{Endpoint: r2srv.URL, Bucket: "b", AccessKeyID: "id", SecretAccessKey: "secret"}
	}})
	ctx := context.Background()
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 100})
	a.connect(nil)
	c := sandboxClient(h, f.apiKey)
	sc := ownsandv1connect.NewSnapshotServiceClient(h.client, h.url, bearer(f.apiKey))
	resp, err := c.CreateSandbox(ctx, createReq(f.org, nil))
	must(t, err)
	sbID := resp.Msg.Sandbox.SandboxId
	a.command()
	a.report(sbID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)

	snap, err := sc.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: f.org, SandboxId: sbID}))
	must(t, err)
	snapID := snap.Msg.Snapshot.SnapshotId
	cmd := a.command()
	if cmd.GetCreateSnapshot().GetSnapshotId() != snapID {
		t.Fatalf("snapshot command: %v", cmd)
	}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: cmd.CommandId, Result: &ownsandv1.AgentCommandResult_Snapshot{Snapshot: &ownsandv1.SnapshotReport{SnapshotId: snapID, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY, SizeBytes: 1 << 30}},
	}}})
	waitSnapshot := func(pred func(*ownsandv1.Snapshot) bool) *ownsandv1.Snapshot {
		for i := 0; i < 100; i++ {
			got, err := sc.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: f.org, SnapshotId: snapID}))
			must(t, err)
			if pred(got.Msg.Snapshot) {
				return got.Msg.Snapshot
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("snapshot did not reach the expected state")
		return nil
	}
	waitSnapshot(func(s *ownsandv1.Snapshot) bool { return s.State == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY })

	tmpl, err := sc.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: f.org, SnapshotId: snapID, Name: "base"}))
	must(t, err)
	if _, err := sc.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: f.org, SnapshotId: snapID, Name: "base"})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate template: %v", err)
	}
	if _, err := sc.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: f.org, SnapshotId: snapID})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete referenced snapshot: %v", err)
	}
	// A sandbox from the template runs on the snapshot's device.
	fromTmpl, err := c.CreateSandbox(ctx, createReq(f.org, func(r *ownsandv1.CreateSandboxRequest) {
		r.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: tmpl.Msg.Template.TemplateId}}
	}))
	must(t, err)
	if src := a.command().GetCreateSandbox().GetSource().GetSnapshotId(); src != snapID || fromTmpl.Msg.Sandbox.DeviceId != a.deviceID {
		t.Fatalf("template source: %s", src)
	}

	// Export: the agent asks for upload URLs; another device gets none.
	exp, err := sc.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: f.org, SnapshotId: snapID}))
	if err != nil || exp.Msg.Snapshot.ExportState != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("export: %v %v", exp, err)
	}
	a.command()
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{RequestId: "r1", SnapshotId: snapID, PartNumbers: []int32{1, 2}, Upload: true}}})
	grant := a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignedUrlGrant().GetRequestId() == "r1" }).GetSignedUrlGrant()
	if grant.ErrorCode != "" || len(grant.Urls) != 2 || !strings.Contains(grant.Urls[1], "/b/organizations/"+f.org+"/snapshots/"+snapID+"/part-00001?") {
		t.Fatalf("grant: %v", grant)
	}
	b := h.enrollAgent(f.org, f.ownerSub)
	b.connect(nil)
	b.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{RequestId: "r2", SnapshotId: snapID, PartNumbers: []int32{1}, Upload: true}}})
	if g := b.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignedUrlGrant().GetRequestId() == "r2" }).GetSignedUrlGrant(); g.ErrorCode == "" || len(g.Urls) != 0 {
		t.Fatalf("other device got upload URLs: %v", g)
	}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SnapshotUpdate{SnapshotUpdate: &ownsandv1.SnapshotUpdate{
		Snapshot:    &ownsandv1.SnapshotReport{SnapshotId: snapID, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY, SizeBytes: 1 << 30},
		ExportState: ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, PartSha256: [][]byte{make([]byte, 32), make([]byte, 32)},
	}}})
	waitSnapshot(func(s *ownsandv1.Snapshot) bool {
		return s.ExportState == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	// The second device may now download.
	b.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{RequestId: "r3", SnapshotId: snapID, PartNumbers: []int32{1}}}})
	if g := b.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignedUrlGrant().GetRequestId() == "r3" }).GetSignedUrlGrant(); g.ErrorCode != "" || len(g.Urls) != 1 {
		t.Fatalf("download grant: %v", g)
	}
	// Deleting the exported snapshot records an R2 clean-up job.
	_, err = sc.DeleteTemplate(ctx, connect.NewRequest(&ownsandv1.DeleteTemplateRequest{OrganizationId: f.org, TemplateId: tmpl.Msg.Template.TemplateId}))
	must(t, err)
	_, err = sc.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: f.org, SnapshotId: snapID}))
	must(t, err)
	jobs, err := h.store.DueCleanupJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Prefix != store.SnapshotPrefix(f.org, snapID) {
		t.Fatalf("clean-up job: %v %v", jobs, err)
	}
	_ = puts
}
