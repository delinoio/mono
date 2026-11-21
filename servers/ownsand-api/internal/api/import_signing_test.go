// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// downloadCountingStore counts the download signer calls of an in-memory
// store, so a test can prove that a denied request never reached the signer.
type downloadCountingStore struct {
	*objectstore.Memory
	gets atomic.Int64
}

// PresignGet implements objectstore.Store.
func (s *downloadCountingStore) PresignGet(key string, ttl time.Duration) (string, time.Time, error) {
	s.gets.Add(1)
	return s.Memory.PresignGet(key, ttl)
}

// importSigningEnv starts a control plane with a counting object store and
// the given import wait.
func importSigningEnv(t *testing.T, importWait time.Duration) (*testenv.Env, *downloadCountingStore) {
	t.Helper()
	store := &downloadCountingStore{}
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: importWait}
	env := testenv.New(t, func(o *app.Options) {
		store.Memory = objectstore.NewMemory(o.Now)
		o.Objects = store
		o.Timing = &timing
	})
	return env, store
}

// requestDownloadURLs asks for the download URLs of parts 1 and 2 and
// returns the grant.
func requestDownloadURLs(agent *testenv.FakeAgent, snapshotID, requestID string) *ownsandv1.SignedUrlGrant {
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{
		RequestId: requestID, SnapshotId: snapshotID, Operation: ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, PartNumbers: []int32{1, 2},
	}}})
	return waitGrant(agent, requestID)
}

// requireDenied checks that a grant is a failed-precondition refusal without
// URLs.
func requireDenied(t *testing.T, grant *ownsandv1.SignedUrlGrant) {
	t.Helper()
	if grant.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION || len(grant.GetPartUrls()) != 0 {
		t.Fatalf("grant = %v, want a failed-precondition error and no URLs", grant)
	}
}

// withholdImportResult makes the fake agent keep the import result back and
// returns a function that returns the import command ID.
func withholdImportResult(agent *testenv.FakeAgent) (importCommandID func() string) {
	var mu sync.Mutex
	var id string
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetImportSnapshot() != nil {
			mu.Lock()
			id = cmd.GetCommandId()
			mu.Unlock()
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return id
	}
}

func sendImportResult(agent *testenv.FakeAgent, commandID string) {
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: commandID, Result: &ownsandv1.AgentCommandResult_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotResult{}},
	}}})
}

// TestDownloadURLsNeedAssignedImport checks that a compatible device without
// an assigned import gets an error grant and no URL, and that the signer is
// never called.
func TestDownloadURLsNeedAssignedImport(t *testing.T) {
	env, store := importSigningEnv(t, 10*time.Second)
	org := env.NewOrg("alice")
	snapID, _, agentB := importFixture(t, env, org)

	requireDenied(t, requestDownloadURLs(agentB, snapID, "no-assignment"))
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("download signer called %d times", got)
	}
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"signed_url_denied"`, `"device_id":"`, `"snapshot_id":"` + snapID + `"`, `"reason":"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s", want)
		}
	}
	if strings.Contains(logs, "memory://") {
		t.Fatal("a signed URL appears in the logs")
	}
}

// TestDownloadURLsDuringAssignedImport checks that the channel that runs the
// import gets URLs (and fresh URLs for a refresh), that another device of the
// organization does not, and that the channel gets none after the result.
func TestDownloadURLsDuringAssignedImport(t *testing.T) {
	env, store := importSigningEnv(t, 10*time.Second)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	_, agentC := setupAgent(t, env, org, nil)
	importCommandID := withholdImportResult(agentB)

	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })

	for _, id := range []string{"first", "refresh"} {
		grant := requestDownloadURLs(agentB, snapID, id)
		if grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
			t.Fatalf("%s grant = %v", id, grant)
		}
	}
	if got := store.gets.Load(); got != 4 {
		t.Fatalf("signer calls after two grants = %d, want 4", got)
	}
	// A different device of the organization with the same platform is not
	// the importing device.
	requireDenied(t, requestDownloadURLs(agentC, snapID, "other-device"))
	if got := store.gets.Load(); got != 4 {
		t.Fatalf("signer called for the other device: %d", got)
	}

	sendImportResult(agentB, importCommandID())
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	requireDenied(t, requestDownloadURLs(agentB, snapID, "after-result"))
	if got := store.gets.Load(); got != 4 {
		t.Fatalf("signer called after the import ended: %d", got)
	}
}

// TestDownloadURLsDeniedToReplacementChannel checks that a new channel of the
// importing device gets no URL without a new assignment.
func TestDownloadURLsDeniedToReplacementChannel(t *testing.T) {
	env, store := importSigningEnv(t, 10*time.Second)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	withholdImportResult(agentB)

	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID))); err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })
	if grant := requestDownloadURLs(agentB, snapID, "original"); grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
		t.Fatalf("original channel grant = %v", grant)
	}
	before := store.gets.Load()

	agent2 := env.NewFakeAgent(devB)
	agent2.Connect()
	select {
	case <-agentB.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the original channel stayed open")
	}
	requireDenied(t, requestDownloadURLs(agent2, snapID, "replacement"))
	if got := store.gets.Load(); got != before {
		t.Fatalf("signer called for the replacement channel: %d, want %d", got, before)
	}
}

// TestDownloadURLsDeniedAfterImportTimeout checks that the assignment ends
// with the import wait.
func TestDownloadURLsDeniedAfterImportTimeout(t *testing.T) {
	env, store := importSigningEnv(t, 300*time.Millisecond)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	withholdImportResult(agentB)

	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	requireDenied(t, requestDownloadURLs(agentB, snapID, "after-timeout"))
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("download signer called %d times", got)
	}
}
