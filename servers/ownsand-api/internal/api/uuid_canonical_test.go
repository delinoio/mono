// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// The contract accepts either hex case in UUID v7 fields, treats two
// spellings of one UUID as one identifier and returns lowercase text. These
// tests send the uppercase spelling and check every place that compares or
// stores the identifier as text.

func mustUpper(t *testing.T, id string) string {
	t.Helper()
	upper := strings.ToUpper(id)
	if upper == id {
		t.Fatalf("id %q has no hex letter to uppercase", id)
	}
	return upper
}

// TestUppercaseIDsInDeletionCleanupPrefixes checks that the R2 clean-up jobs
// of DeleteSnapshot and DeleteOrganization use the lowercase prefixes that the
// signed object keys use, whichever case the caller wrote.
func TestUppercaseIDsInDeletionCleanupPrefixes(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: mustUpper(t, org.ID), SandboxId: mustUpper(t, sandboxID)}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	if snap.Msg.GetSnapshot().GetOrganizationId() != org.ID || snap.Msg.GetSnapshot().GetSourceSandboxId() != sandboxID {
		t.Fatalf("snapshot = %v", snap.Msg.GetSnapshot())
	}
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: mustUpper(t, org.ID), SnapshotId: mustUpper(t, snapID)}))
		return err == nil && s.Msg.GetSnapshot().GetSnapshotId() == snapID && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: mustUpper(t, org.ID), SnapshotId: mustUpper(t, snapID)})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("signed urls", func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignedUrlGrant() != nil })
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: mustUpper(t, org.ID), SnapshotId: mustUpper(t, snapID)})); err != nil {
		t.Fatal(err)
	}
	var prefix string
	if err := env.DB.Pool.QueryRow(ctx, "SELECT prefix FROM r2_cleanup_jobs WHERE organization_id = $1", org.ID).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if want := "organizations/" + org.ID + "/snapshots/" + snapID + "/"; prefix != want {
		t.Fatalf("snapshot clean-up prefix = %q, want %q", prefix, want)
	}

	doomed := env.NewOrg("bob")
	if _, err := doomed.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: mustUpper(t, doomed.ID)})); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT prefix FROM r2_cleanup_jobs WHERE organization_id = $1", doomed.ID).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if want := "organizations/" + doomed.ID + "/"; prefix != want {
		t.Fatalf("organization clean-up prefix = %q, want %q", prefix, want)
	}
}

// TestUppercaseIDsInSandboxRPCs checks that CreateSandbox places on an
// uppercase placement.device_id and replays an idempotency key across hex
// cases, that the agent commands, the audit target and the responses carry
// lowercase identifiers, and that malformed identifiers stay invalid.
func TestUppercaseIDsInSandboxRPCs(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient

	upper := createReq(org.ID)
	upper.OrganizationId = mustUpper(t, org.ID)
	upper.IdempotencyKey = mustUpper(t, upper.IdempotencyKey)
	upper.Placement = &ownsandv1.Placement{DeviceId: proto.String(mustUpper(t, dev.ID))}
	first, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(upper))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := first.Msg.GetSandbox().GetSandboxId()
	if first.Msg.GetSandbox().GetDeviceId() != dev.ID || first.Msg.GetSandbox().GetOrganizationId() != org.ID {
		t.Fatalf("created = %v", first.Msg.GetSandbox())
	}
	// The lowercase spelling of the same request replays the sandbox.
	lower := proto.Clone(upper).(*ownsandv1.CreateSandboxRequest)
	lower.OrganizationId = org.ID
	lower.IdempotencyKey = strings.ToLower(upper.IdempotencyKey)
	lower.Placement = &ownsandv1.Placement{DeviceId: proto.String(dev.ID)}
	again, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(lower))
	if err != nil || again.Msg.GetSandbox().GetSandboxId() != sandboxID {
		t.Fatalf("replay across hex cases: %v %v", again, err)
	}
	// Source IDs written in uppercase resolve too.
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	fromSnapshot := createReq(org.ID)
	fromSnapshot.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: mustUpper(t, snapID)}}
	fromSnapshot.Resources.DiskGib = 10
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(fromSnapshot)); err != nil {
		t.Fatalf("uppercase source.snapshot_id: %v", err)
	}
	// Malformed identifiers stay invalid_argument.
	bad := createReq(org.ID)
	bad.Placement = &ownsandv1.Placement{DeviceId: proto.String("not-a-uuid")}
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(bad)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed device_id: %v", err)
	}

	upperIDs := func(orgID, id string) (string, string) { return mustUpper(t, orgID), mustUpper(t, id) }
	o, s := upperIDs(org.ID, sandboxID)
	paused, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: o, SandboxId: s}))
	if err != nil || paused.Msg.GetSandbox().GetSandboxId() != sandboxID {
		t.Fatalf("pause: %v %v", paused, err)
	}
	agent.WaitFor("pause command with the lowercase id", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetPauseSandbox().GetSandboxId() == sandboxID
	})
	if _, err := c.Sandbox.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: o, SandboxId: s})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("resume command with the lowercase id", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetResumeSandbox().GetSandboxId() == sandboxID
	})
	got, err := c.Sandbox.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: o, SandboxId: s}))
	if err != nil || got.Msg.GetSandbox().GetSandboxId() != sandboxID {
		t.Fatalf("get: %v %v", got, err)
	}
	// The wait of DeleteSandbox subscribes under the lowercase ID that the
	// state report notifies; a waiter under another spelling would only wake
	// at the one-second poll.
	start := time.Now()
	deleted, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: o, SandboxId: s}))
	if err != nil || deleted.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
		t.Fatalf("delete woke after %v, which is the poll interval, not the notification", elapsed)
	}
	var upperTargets int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND target_id <> lower(target_id)", org.ID).Scan(&upperTargets); err != nil {
		t.Fatal(err)
	}
	if upperTargets != 0 {
		t.Fatalf("%d audit events have an uppercase target_id", upperTargets)
	}
	var deletedAudits int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 18 AND target_id = $1", sandboxID).Scan(&deletedAudits)
	if deletedAudits != 1 {
		t.Fatalf("sandbox deleted audit events with the lowercase target = %d", deletedAudits)
	}
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: o, SandboxId: "not-a-uuid"})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed sandbox_id: %v", err)
	}
}

// TestUppercaseIDsInOrganizationRPCs checks that responses and audit targets
// of the organization-scoped RPCs carry the lowercase organization ID.
func TestUppercaseIDsInOrganizationRPCs(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	upper := mustUpper(t, org.ID)

	got, err := c.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: upper}))
	if err != nil || got.Msg.GetOrganization().GetOrganizationId() != org.ID {
		t.Fatalf("get organization: %v %v", got, err)
	}
	members, err := c.Member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: upper}))
	if err != nil || len(members.Msg.GetMembers()) == 0 {
		t.Fatalf("list members: %v %v", members, err)
	}
	for _, m := range members.Msg.GetMembers() {
		if m.GetOrganizationId() != org.ID {
			t.Fatalf("member organization_id = %q", m.GetOrganizationId())
		}
	}
	policy, err := c.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{
		OrganizationId: upper, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD,
	}))
	if err != nil || policy.Msg.GetPolicy().GetOrganizationId() != org.ID {
		t.Fatalf("update policy: %v %v", policy, err)
	}
	var upperTargets int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND target_id <> lower(target_id)", org.ID).Scan(&upperTargets); err != nil {
		t.Fatal(err)
	}
	if upperTargets != 0 {
		t.Fatalf("%d audit events have an uppercase target_id", upperTargets)
	}
	if _, err := c.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: "not-a-uuid"})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed organization_id: %v", err)
	}
}

// TestUppercaseIDsOnRelay checks that AuthorizeRelay and the relay RPCs match
// the grant by value across hex cases, that the agent sees one lowercase
// spelling of the grant and of the forwarded request, and that exec_id stays
// lowercase-only.
func TestUppercaseIDsOnRelay(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	forwarded := make(chan *ownsandv1.DataFrame, 8)
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		forwarded <- f
		switch {
		case f.GetExecRequest() != nil:
			serveExec(s, f)
		case f.GetFileReadRequest() != nil:
			s.Send(&ownsandv1.RequestAccepted{})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 0, Data: []byte("file")}}})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: 4}}})
		case f.GetFileWriteChunk() != nil:
			w := f.GetFileWriteChunk()
			s.Send(&ownsandv1.WriteFileResponse{WriteId: w.GetWriteId(), Offset: w.GetOffset(), Length: int64(len(w.GetData()))})
		}
	})
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	key := env.Clients(org.APIKey)
	upperOrg, upperSandbox := mustUpper(t, org.ID), mustUpper(t, sandboxID)

	// ReadFile: authorized with the uppercase spelling, called with the
	// uppercase spelling. The digest binds the lowercase text that the agent
	// receives.
	readID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	read := &ownsandv1.ReadFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, ReadId: readID, Path: "/etc/x", BindingSalt: salt}
	authID, _ := uuidv7.NewString()
	grant, err := key.DataPath.AuthorizeRelay(ctx, connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
		OrganizationId: upperOrg, SandboxId: upperSandbox, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE,
		OperationId: mustUpper(t, readID), RequestDigest: relaybinding.ReadFileDigest(read), AuthorizationId: mustUpper(t, authID),
	}))
	if err != nil {
		t.Fatalf("authorize with uppercase ids: %v", err)
	}
	g := agent.WaitFor("relay grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == grant.Msg.GetGrantId() }).GetRelayGrant()
	if g.GetOperationId() != readID || g.GetSandboxId() != sandboxID {
		t.Fatalf("grant for the agent = %v", g)
	}
	// A retry with the lowercase authorization_id returns the same grant.
	retry, err := key.DataPath.AuthorizeRelay(ctx, connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
		OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE,
		OperationId: readID, RequestDigest: relaybinding.ReadFileDigest(read), AuthorizationId: authID,
	}))
	if err != nil || retry.Msg.GetGrantId() != grant.Msg.GetGrantId() {
		t.Fatalf("retry across hex cases: %v %v", retry, err)
	}
	upperRead := proto.Clone(read).(*ownsandv1.ReadFileRequest)
	upperRead.OrganizationId, upperRead.SandboxId, upperRead.ReadId = upperOrg, upperSandbox, mustUpper(t, readID)
	rs, err := env.ClientsWith(org.APIKey, grantHeader(grant.Msg.GetGrantId())).Relay.ReadFile(ctx, connect.NewRequest(upperRead))
	if err != nil {
		t.Fatal(err)
	}
	for rs.Receive() {
	}
	if rs.Err() != nil {
		t.Fatalf("read with uppercase ids: %v", rs.Err())
	}
	got := (<-forwarded).GetFileReadRequest()
	if got.GetReadId() != readID || got.GetSandboxId() != sandboxID || got.GetOrganizationId() != org.ID {
		t.Fatalf("forwarded read request = %v", got)
	}

	// A replacement names the predecessor in uppercase.
	prev := mustUpper(t, grant.Msg.GetGrantId())
	replAuth, _ := uuidv7.NewString()
	repl, err := key.DataPath.AuthorizeRelay(ctx, connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
		OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE,
		OperationId: mustUpper(t, readID), RequestDigest: relaybinding.ReadFileDigest(read), AuthorizationId: replAuth, ReplacesGrantId: &prev,
	}))
	if err != nil {
		t.Fatalf("replacement with an uppercase replaces_grant_id: %v", err)
	}
	rg := agent.WaitFor("replacement grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == repl.Msg.GetGrantId() }).GetRelayGrant()
	if rg.GetReplacesGrantId() != grant.Msg.GetGrantId() {
		t.Fatalf("replacement grant = %v", rg)
	}

	// WriteFile with uppercase ids.
	writeID, _ := uuidv7.NewString()
	write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: []byte("chunk")}
	wAuth := authorize(t, key, upperOrg, upperSandbox, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, mustUpper(t, writeID), relaybinding.WriteFileDigest(write))
	upperWrite := proto.Clone(write).(*ownsandv1.WriteFileRequest)
	upperWrite.OrganizationId, upperWrite.SandboxId, upperWrite.WriteId = upperOrg, upperSandbox, mustUpper(t, writeID)
	if wr, err := env.ClientsWith(org.APIKey, grantHeader(wAuth)).Relay.WriteFile(ctx, connect.NewRequest(upperWrite)); err != nil || wr.Msg.GetLength() != 5 {
		t.Fatalf("write with uppercase ids: %v %v", wr, err)
	}
	if got := (<-forwarded).GetFileWriteChunk(); got.GetWriteId() != writeID || got.GetSandboxId() != sandboxID {
		t.Fatalf("forwarded write request = %v", got)
	}

	// Exec: the organization and sandbox match by value, exec_id stays
	// lowercase-only.
	execReq := newExec(org.ID, sandboxID)
	execGrant, err := authorizeExec(t, key, execReq)
	if err != nil {
		t.Fatal(err)
	}
	upperExec := proto.Clone(execReq).(*ownsandv1.ExecRequest)
	upperExec.OrganizationId, upperExec.SandboxId = upperOrg, upperSandbox
	stream, err := env.ClientsWith(org.APIKey, grantHeader(execGrant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(upperExec))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collectExec(t, stream); err != nil {
		t.Fatalf("exec with an uppercase organization and sandbox: %v", err)
	}
	if got := (<-forwarded).GetExecRequest(); got.GetSandboxId() != sandboxID || got.GetOrganizationId() != org.ID || got.GetExecId() != execReq.GetExecId() {
		t.Fatalf("forwarded exec request = %v", got)
	}
	badExec := proto.Clone(execReq).(*ownsandv1.ExecRequest)
	badExec.ExecId = mustUpper(t, execReq.GetExecId())
	stream, err = env.ClientsWith(org.APIKey, grantHeader(execGrant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(badExec))
	if err == nil {
		_, err = collectExec(t, stream)
	}
	if testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("exec with an uppercase exec_id: %v", err)
	}
}

// watcherWakeBound is the longest wait that still proves the in-process
// watcher woke a waiting RPC. The wait falls back to a one second poll, so a
// wake that needs the poll is slower than this bound.
const watcherWakeBound = 800 * time.Millisecond

// deleteWaiter is a DeleteSandbox call that waits for the agent.
type deleteWaiter struct {
	done    chan struct{}
	resp    *connect.Response[ownsandv1.DeleteSandboxResponse]
	err     error
	started time.Time
	elapsed time.Duration
}

// startDeleteWaiter calls DeleteSandbox and returns once the call waits on its
// watcher. The call returns when the watcher wakes it, the command wait ends
// or the one second poll finds the terminal state.
func startDeleteWaiter(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string) *deleteWaiter {
	t.Helper()
	w := &deleteWaiter{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.resp, w.err = org.OwnerClient.Sandbox.DeleteSandbox(context.Background(), connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	}()
	testenv.Eventually(t, "delete waiter registered", func() bool { return env.App.Service.WatcherCount() == 1 })
	// The poll ticker started with the waiter, so measure from here.
	w.started = time.Now()
	return w
}

// wait returns whether the call returned within the bound and records how
// long the call took.
func (w *deleteWaiter) wait(bound time.Duration) bool {
	select {
	case <-w.done:
		w.elapsed = time.Since(w.started)
		return true
	case <-time.After(bound):
		return false
	}
}

func longCommandWaitEnv(t *testing.T) *testenv.Env {
	t.Helper()
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 20 * time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	return testenv.New(t, func(o *app.Options) { o.Timing = &timing })
}

// withUpperID returns a copy of e that spells the sandbox ID in uppercase.
func withUpperID(t *testing.T, e *ownsandv1.SandboxStateEntry) *ownsandv1.SandboxStateEntry {
	t.Helper()
	out := proto.Clone(e).(*ownsandv1.SandboxStateEntry)
	out.SandboxId = mustUpper(t, e.GetSandboxId())
	return out
}

// TestUppercaseSandboxIDInCommandResultWakesWaiter checks that a DeleteSandbox
// waits on its watcher and that a command result whose sandbox_id is in
// uppercase wakes it at once, not at the next one second poll. The watchers
// are keyed by the lowercase ID.
func TestUppercaseSandboxIDInCommandResultWakesWaiter(t *testing.T) {
	env := longCommandWaitEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// The agent answers the delete after the waiter registered, with the
	// uppercase spelling of the ID.
	release := make(chan struct{})
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetDeleteSandbox() == nil {
			return fa.DefaultAnswer(cmd)
		}
		<-release
		res := fa.DefaultAnswer(cmd)
		res.GetSandbox().Sandbox = withUpperID(t, res.GetSandbox().GetSandbox())
		return res
	})
	w := startDeleteWaiter(t, env, org, id)
	close(release)
	if !w.wait(5 * time.Second) {
		t.Fatal("the delete did not return")
	}
	if w.err != nil {
		t.Fatal(w.err)
	}
	if w.elapsed > watcherWakeBound {
		t.Fatalf("the delete returned after %v; the watcher did not wake it", w.elapsed)
	}
	if sb := w.resp.Msg.GetSandbox(); sb.GetSandboxId() != id || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("deleted = %v", sb)
	}
}

// TestUppercaseSandboxIDInSandboxUpdateWakesWaiter checks that a SandboxUpdate
// with an uppercase sandbox_id and a newer revision wakes a waiting
// DeleteSandbox at once, and that the response and the stored row keep the
// lowercase ID.
func TestUppercaseSandboxIDInSandboxUpdateWakesWaiter(t *testing.T) {
	env := longCommandWaitEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// The agent never answers the delete, so only an update can end the wait.
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetDeleteSandbox() != nil {
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
	w := startDeleteWaiter(t, env, org, id)
	deleted := agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
	agent.SendUpdate(withUpperID(t, deleted))
	if !w.wait(5 * time.Second) {
		t.Fatal("the delete did not return")
	}
	if w.err != nil {
		t.Fatal(w.err)
	}
	if w.elapsed > watcherWakeBound {
		t.Fatalf("the delete returned after %v; the watcher did not wake it", w.elapsed)
	}
	if sb := w.resp.Msg.GetSandbox(); sb.GetSandboxId() != id || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("deleted = %v", sb)
	}
	var stored int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE sandbox_id = $1 AND sandbox_id::text = $2", id, id).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored sandbox rows = %d, %v", stored, err)
	}
}

// TestUppercaseStaleAndUnknownSandboxUpdatesKeepWaiter checks that an
// uppercase update with a stale revision and an uppercase update of an
// unknown sandbox are acknowledged, change nothing and leave a waiting
// DeleteSandbox waiting.
func TestUppercaseStaleAndUnknownSandboxUpdatesKeepWaiter(t *testing.T) {
	env := longCommandWaitEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetDeleteSandbox() != nil {
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
	w := startDeleteWaiter(t, env, org, id)

	current := agent.Sandbox(id)
	stale := withUpperID(t, current)
	stale.State = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	stale.Revision.Counter = current.GetRevision().GetCounter() - 1
	agent.SendUpdate(stale)
	agent.WaitFor("stale ack", func(e *ownsandv1.AgentEnvelope) bool {
		a := e.GetStateAck().GetSandboxUpdate()
		return strings.EqualFold(a.GetSandboxId(), id) && a.GetRevision().GetCounter() == stale.GetRevision().GetCounter()
	})
	unknownID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	unknown := withUpperID(t, current)
	unknown.SandboxId = mustUpper(t, unknownID)
	unknown.Revision.Counter = current.GetRevision().GetCounter() + 100
	agent.SendUpdate(unknown)
	agent.WaitFor("unknown ack", func(e *ownsandv1.AgentEnvelope) bool {
		return strings.EqualFold(e.GetStateAck().GetSandboxUpdate().GetSandboxId(), unknownID)
	})
	if w.wait(300 * time.Millisecond) {
		t.Fatalf("a stale or unknown update ended the delete: %v %v", w.resp, w.err)
	}
	// The delete is pending, so the state is DELETING; the stale PAUSED
	// state must not show.
	if st := getSandbox(t, c, org.ID, id).GetState(); st == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || st == ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("stale update applied: %v", st)
	}
	// End the waiter so the test does not hold a call for the command wait.
	agent.SendUpdate(agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER))
	if !w.wait(5 * time.Second) {
		t.Fatal("the delete did not return")
	}
}

// TestLowercaseSandboxIDInSandboxUpdateWakesWaiter keeps the lowercase
// spelling working: it wakes a waiting DeleteSandbox at once.
func TestLowercaseSandboxIDInSandboxUpdateWakesWaiter(t *testing.T) {
	env := longCommandWaitEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetDeleteSandbox() != nil {
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
	w := startDeleteWaiter(t, env, org, id)
	deleted := agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
	agent.SendUpdate(deleted)
	// w.err and w.resp are written by the call goroutine before it closes
	// w.done, so read them only after wait returned true.
	if !w.wait(5 * time.Second) {
		t.Fatal("the delete did not return within 5s; the watcher did not wake the waiter")
	}
	if w.err != nil {
		t.Fatalf("the delete failed: %v", w.err)
	}
	if w.elapsed > watcherWakeBound {
		t.Fatalf("the delete returned after %v; the watcher did not wake it", w.elapsed)
	}
}

// TestUppercaseSandboxIDInPauseResumeResults checks that pause and resume
// results with an uppercase sandbox_id are applied to the lowercase row and
// answered with the lowercase ID.
func TestUppercaseSandboxIDInPauseResumeResults(t *testing.T) {
	env := longCommandWaitEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		res := fa.DefaultAnswer(cmd)
		if r := res.GetSandbox(); r != nil && (cmd.GetPauseSandbox() != nil || cmd.GetResumeSandbox() != nil) {
			r.Sandbox = withUpperID(t, r.GetSandbox())
		}
		return res
	})
	start := time.Now()
	paused, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: id}))
	if err != nil || paused.Msg.GetSandbox().GetSandboxId() != id || paused.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause: %v %v", paused, err)
	}
	resumed, err := c.Sandbox.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: org.ID, SandboxId: id}))
	if err != nil || resumed.Msg.GetSandbox().GetSandboxId() != id || resumed.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("pause and resume took %v", elapsed)
	}
}
