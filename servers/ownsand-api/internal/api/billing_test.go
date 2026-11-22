// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

const (
	billingAccountA = "01900000-0000-7000-8000-0000000000c1"
	billingAccountB = "01900000-0000-7000-8000-0000000000c2"
	billingAccountC = "01900000-0000-7000-8000-0000000000c3"
)

// billingEnv starts a control plane with a fake delino-accounts on the
// test clock. The background status and suspension workers run rarely, so
// the tests drive the poll and the suspension passes.
func billingEnv(t *testing.T) (*testenv.Env, *accountstest.Server) {
	t.Helper()
	var env *testenv.Env
	fake := accountstest.New(t, func() time.Time { return env.Clock.Now() })
	env = testenv.New(t, testenv.WithAccounts(fake), func(o *app.Options) {
		o.BillingStatusTiming = &billing.StatusTiming{Poll: time.Hour, Tick: time.Hour, UnlinkBackoffMin: time.Second, UnlinkBackoffMax: 5 * time.Minute}
		o.BillingEnforceInterval = time.Hour
		// No usage report in the background: its statuses would refresh the
		// sync time that the fail-open test lets age.
		o.BillingTiming = &billing.Timing{Flush: time.Hour, Tick: time.Hour, Report: time.Hour, BackoffMin: time.Second, BackoffMax: time.Minute}
	})
	return env, fake
}

// linkedOrg creates an organization whose owner links it to a new billing
// account in good standing in which the owner is the billing owner.
func linkedOrg(t *testing.T, env *testenv.Env, fake *accountstest.Server, owner, account string) *testenv.Org {
	t.Helper()
	fake.AddAccount(account, "Account of "+owner, map[string]accountsv1.BillingAccountRole{owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	org := env.NewUnlinkedOrg(owner)
	resp, err := org.OwnerClient.Billing.LinkBillingAccount(context.Background(), connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: account}))
	if err != nil {
		t.Fatalf("link billing account: %v", err)
	}
	if resp.Msg.GetBilling().GetPhase() != billing.PhaseAllowed || resp.Msg.GetBillingAccountName() != "Account of "+owner {
		t.Fatalf("link response = %v", resp.Msg)
	}
	return org
}

func pollBilling(t *testing.T, env *testenv.Env) {
	t.Helper()
	if err := env.App.Billing.Poll(context.Background()); err != nil {
		t.Fatalf("poll billing statuses: %v", err)
	}
}

func enforceBilling(t *testing.T, env *testenv.Env) {
	t.Helper()
	if err := env.App.Enforcer.Pass(context.Background()); err != nil {
		t.Fatalf("billing enforcement pass: %v", err)
	}
}

func billingStatus(t *testing.T, c *testenv.Clients, orgID string) *ownsandv1.OrganizationBilling {
	t.Helper()
	resp, err := c.Billing.GetBillingStatus(context.Background(), connect.NewRequest(&ownsandv1.GetBillingStatusRequest{OrganizationId: orgID}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	return resp.Msg.GetBilling()
}

// billingReasonOf returns the billing reason of a billing error and checks
// its shape: resource_exhausted, an ErrorDetail with the code, a message
// with the accounts link and no retry delay. It returns UNSPECIFIED for any
// other error.
func billingReasonOf(t *testing.T, name string, err error) ownsandv1.BillingReason {
	t.Helper()
	d := errDetail(err)
	if d.GetBillingReason() == ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
		return ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED
	}
	if testenv.Code(err) != connect.CodeResourceExhausted || d.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED ||
		!strings.Contains(d.GetMessage(), "https://accounts.delino.io") || d.GetRetryAfter() != nil {
		t.Fatalf("%s: billing error shape: %v (detail %v)", name, err, d)
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message(), "https://accounts.delino.io") {
		t.Fatalf("%s: message without the accounts link: %v", name, err)
	}
	return d.GetBillingReason()
}

// drainStream returns the error of a server stream.
func drainStream[T any](s *connect.ServerStreamForClient[T], err error) error {
	if err != nil {
		return err
	}
	for s.Receive() {
	}
	return s.Err()
}

// gateFixture holds what the gated calls need.
type gateFixture struct {
	env       *testenv.Env
	org       *testenv.Org
	sandboxID string
	// pausedID is a sandbox that a caller paused; PauseSandbox on it
	// changes nothing.
	pausedID string
	// enrollToken was created while the organization was allowed.
	enrollToken string
	// grantID is an exec grant of the API key that no call used.
	grantID string
}

type gatedCall struct {
	name string
	call func(ctx context.Context) error
}

// blockedCalls are the RPCs that BLOCKED rejects.
func (g *gateFixture) blockedCalls() []gatedCall {
	c := g.org.OwnerClient
	o := g.org.ID
	missing := "01900000-0000-7000-8000-00000000dead"
	return []gatedCall{
		{"CreateSandbox", func(ctx context.Context) error {
			_, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(o)))
			return err
		}},
		{"ResumeSandbox", func(ctx context.Context) error {
			_, err := c.Sandbox.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: o, SandboxId: g.sandboxID}))
			return err
		}},
		{"CreateSnapshot", func(ctx context.Context) error {
			_, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: o, SandboxId: missing}))
			return err
		}},
		{"ExportSnapshot", func(ctx context.Context) error {
			_, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: o, SnapshotId: missing}))
			return err
		}},
		{"CreateTemplate", func(ctx context.Context) error {
			_, err := c.Snapshot.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: o, SnapshotId: missing, Name: "t"}))
			return err
		}},
		{"CreateEnrollmentToken", func(ctx context.Context) error {
			_, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: o, Ttl: durationpb.New(time.Hour)}))
			return err
		}},
		{"Enroll", func(ctx context.Context) error {
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			_, err := g.env.Clients("").Agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
				EnrollmentToken: g.enrollToken, PublicKey: pub, Name: "late", HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0",
			}))
			return err
		}},
	}
}

// suspendedCalls are the RPCs that only SUSPENDED rejects. The relay RPCs
// use an unused grant; the gate rejects them before the grant is matched
// to the request.
func (g *gateFixture) suspendedCalls() []gatedCall {
	key := g.env.Clients(g.org.APIKey)
	relay := g.env.ClientsWith(g.org.APIKey, grantHeader(g.grantID)).Relay
	o, sb := g.org.ID, g.sandboxID
	id := func() string { v, _ := uuidv7.NewString(); return v }
	return []gatedCall{
		{"Exec", func(ctx context.Context) error {
			return drainStream(relay.Exec(ctx, connect.NewRequest(newExec(o, sb))))
		}},
		{"AttachExec", func(ctx context.Context) error {
			return drainStream(relay.AttachExec(ctx, connect.NewRequest(&ownsandv1.AttachExecRequest{OrganizationId: o, SandboxId: sb, ExecId: id()})))
		}},
		{"ReadFile", func(ctx context.Context) error {
			return drainStream(relay.ReadFile(ctx, connect.NewRequest(&ownsandv1.ReadFileRequest{OrganizationId: o, SandboxId: sb, ReadId: id(), Path: "/tmp/x"})))
		}},
		{"WriteFile", func(ctx context.Context) error {
			_, err := relay.WriteFile(ctx, connect.NewRequest(&ownsandv1.WriteFileRequest{OrganizationId: o, SandboxId: sb, WriteId: id(), Path: "/tmp/x", Data: []byte("x")}))
			return err
		}},
		{"OpenSession", func(ctx context.Context) error {
			_, err := key.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: o, SandboxId: sb, SdpOffer: "v=0"}))
			return err
		}},
		{"AuthorizeRelay", func(ctx context.Context) error {
			req := newExec(o, sb)
			_, err := key.DataPath.AuthorizeRelay(ctx, connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{OrganizationId: o, SandboxId: sb,
				OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: id()}))
			return err
		}},
	}
}

// openCalls are RPCs that billing never rejects, BillingService included.
func (g *gateFixture) openCalls() []gatedCall {
	c := g.org.OwnerClient
	o := g.org.ID
	return []gatedCall{
		{"GetSandbox", func(ctx context.Context) error {
			_, err := c.Sandbox.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: o, SandboxId: g.sandboxID}))
			return err
		}},
		{"ListSandboxes", func(ctx context.Context) error {
			_, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: o}))
			return err
		}},
		{"PauseSandbox", func(ctx context.Context) error {
			_, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: o, SandboxId: g.pausedID}))
			return err
		}},
		{"ListSnapshots", func(ctx context.Context) error {
			_, err := c.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: o}))
			return err
		}},
		{"ListDevices", func(ctx context.Context) error {
			_, err := c.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: o}))
			return err
		}},
		{"GetIceServers", func(ctx context.Context) error {
			_, err := c.DataPath.GetIceServers(ctx, connect.NewRequest(&ownsandv1.GetIceServersRequest{OrganizationId: o}))
			return err
		}},
		{"CreateApiKey", func(ctx context.Context) error {
			_, err := c.APIKey.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: o, Name: "while-billing-restricted"}))
			return err
		}},
		{"GetBillingStatus", func(ctx context.Context) error {
			_, err := c.Billing.GetBillingStatus(ctx, connect.NewRequest(&ownsandv1.GetBillingStatusRequest{OrganizationId: o}))
			return err
		}},
		{"GetBillingUsage", func(ctx context.Context) error {
			_, err := c.Billing.GetBillingUsage(ctx, connect.NewRequest(&ownsandv1.GetBillingUsageRequest{OrganizationId: o}))
			return err
		}},
		{"ListLinkableBillingAccounts", func(ctx context.Context) error {
			_, err := c.Billing.ListLinkableBillingAccounts(ctx, connect.NewRequest(&ownsandv1.ListLinkableBillingAccountsRequest{OrganizationId: o}))
			return err
		}},
	}
}

// expectGates checks every gated and open call in the phase: the calls in
// rejected fail with reason, every other gated call has no billing error
// (it may still fail for another reason, such as a missing ID), and every
// open call succeeds.
func (g *gateFixture) expectGates(t *testing.T, rejected map[string]bool, reason ownsandv1.BillingReason) {
	t.Helper()
	ctx := context.Background()
	all := append(append(g.blockedCalls(), g.suspendedCalls()...), g.openCalls()...)
	for _, c := range all {
		err := c.call(ctx)
		got := billingReasonOf(t, c.name, err)
		if rejected[c.name] {
			if got != reason {
				t.Errorf("%s: billing reason = %v, want %v", c.name, got, reason)
			}
			continue
		}
		if got != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
			t.Errorf("%s: unexpected billing reason %v", c.name, got)
		}
	}
	for _, c := range g.openCalls() {
		if err := c.call(ctx); err != nil {
			t.Errorf("%s failed: %v", c.name, err)
		}
	}
}

func names(calls ...[]gatedCall) map[string]bool {
	out := map[string]bool{}
	for _, cs := range calls {
		for _, c := range cs {
			out[c.name] = true
		}
	}
	return out
}

func pausedByBilling(t *testing.T, env *testenv.Env, sandboxID string) bool {
	t.Helper()
	var marked bool
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT paused_by_billing FROM sandboxes WHERE sandbox_id = $1", sandboxID).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	return marked
}

func pauseCommands(agent *testenv.FakeAgent, sandboxID string) int {
	n := 0
	for _, c := range agent.Commands() {
		if c.GetPauseSandbox().GetSandboxId() == sandboxID {
			n++
		}
	}
	return n
}

// TestBillingPhasesOfLinkedOrganization follows a linked organization from
// ALLOWED through BLOCKED and SUSPENDED back to ALLOWED: each phase rejects
// exactly its RPCs with its reason, the suspension pauses the running
// sandboxes and marks them, and the recovery resumes only those.
func TestBillingPhasesOfLinkedOrganization(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	org := linkedOrg(t, env, fake, "alice", billingAccountA)
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	create := func() string {
		created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
		if err != nil {
			t.Fatal(err)
		}
		id := created.Msg.GetSandbox().GetSandboxId()
		waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		return id
	}
	running := create()
	userPaused := create()
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: userPaused})); err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, userPaused, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	tok, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	g := &gateFixture{env: env, org: org, sandboxID: running, pausedID: userPaused, enrollToken: tok.Msg.GetEnrollmentToken()}
	if b := billingStatus(t, env.User(org.Member), org.ID); b.GetPhase() != billing.PhaseAllowed || !b.GetLinked() {
		t.Fatalf("allowed status = %v", b)
	}

	// BLOCKED: the linked billing account ran out of credit.
	blockedAt := env.Clock.Now()
	fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, blockedAt)
	pollBilling(t, env)
	b := billingStatus(t, env.User(org.Member), org.ID)
	if b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_BLOCKED || !b.GetBlockedSince().AsTime().Equal(blockedAt) ||
		!b.GetSuspendTime().AsTime().Equal(blockedAt.Add(72*time.Hour)) {
		t.Fatalf("blocked status = %v", b)
	}
	// The suspended-only RPCs still work: a relay grant, an exec over the
	// relay and a data channel session.
	relay := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		if f.GetExecRequest() != nil {
			serveExec(s, f)
		}
	})
	relay.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	key := env.Clients(org.APIKey)
	execReq := newExec(org.ID, running)
	grant, err := authorizeExec(t, key, execReq)
	if err != nil {
		t.Fatalf("AuthorizeRelay while blocked: %v", err)
	}
	if err := drainStream(env.ClientsWith(org.APIKey, grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(execReq))); err != nil {
		t.Fatalf("Exec while blocked: %v", err)
	}
	if _, err := key.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: running, SdpOffer: "v=0"})); err != nil {
		t.Fatalf("OpenSession while blocked: %v", err)
	}
	unused, err := authorizeExec(t, key, newExec(org.ID, running))
	if err != nil {
		t.Fatal(err)
	}
	g.grantID = unused.GetGrantId()
	g.expectGates(t, names(g.blockedCalls()), ownsandv1.BillingReason_BILLING_REASON_BLOCKED)
	enforceBilling(t, env)
	if pauseCommands(agent, running) != 0 || pausedByBilling(t, env, running) {
		t.Fatal("a blocked organization's sandbox was paused")
	}

	// SUSPENDED: 3 days of non-payment, which delino-accounts reports.
	fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, blockedAt)
	pollBilling(t, env)
	enforceBilling(t, env)
	waitState(t, c, org.ID, running, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if !pausedByBilling(t, env, running) || pausedByBilling(t, env, userPaused) {
		t.Fatalf("billing marks: running=%v userPaused=%v", pausedByBilling(t, env, running), pausedByBilling(t, env, userPaused))
	}
	if b := billingStatus(t, key, org.ID); b.GetPhase() != billing.PhaseSuspended || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("suspended status = %v", b)
	}
	g.expectGates(t, names(g.blockedCalls(), g.suspendedCalls()), ownsandv1.BillingReason_BILLING_REASON_SUSPENDED)
	// Passes are idempotent: a paused sandbox gets no second pause.
	pauses := pauseCommands(agent, running)
	enforceBilling(t, env)
	if got := pauseCommands(agent, running); got != pauses {
		t.Fatalf("pause commands after a second pass = %d, want %d", got, pauses)
	}
	// A sandbox that becomes RUNNING again while the organization stays
	// suspended (for example after its device reconnected) is paused again.
	agent.SendUpdate(agent.SetSandbox(running, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0))
	waitState(t, c, org.ID, running, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	enforceBilling(t, env)
	waitState(t, c, org.ID, running, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if !strings.Contains(env.Logs.String(), `"msg":"billing_sandbox_paused"`) {
		t.Fatal("billing_sandbox_paused not logged")
	}

	// Recovery: only the sandbox that billing paused resumes.
	fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, time.Time{})
	pollBilling(t, env)
	enforceBilling(t, env)
	waitState(t, c, org.ID, running, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	testenv.Eventually(t, "the billing mark is cleared", func() bool { return !pausedByBilling(t, env, running) })
	if s := getSandbox(t, c, org.ID, userPaused); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the caller-paused sandbox is %v", s.GetState())
	}
	if b := billingStatus(t, c, org.ID); b.GetPhase() != billing.PhaseAllowed || b.GetBlockedSince() != nil {
		t.Fatalf("recovered status = %v", b)
	}
	g.expectGates(t, nil, 0)
	// The device is full (two sandboxes hold its vCPUs), so the create
	// fails, but not for billing.
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); billingReasonOf(t, "CreateSandbox", err) != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
		t.Fatalf("CreateSandbox after the recovery: %v", err)
	}
	for _, msg := range []string{`"msg":"billing_sandbox_resumed"`, `"msg":"billing_status_changed"`, `"to_phase":"suspended"`, `"to_phase":"allowed"`} {
		if !strings.Contains(env.Logs.String(), msg) {
			t.Fatalf("log %s missing", msg)
		}
	}
}

// TestBillingUnlinkedOrganization checks the local status of an
// organization without a link: blocked with ACCOUNT_REQUIRED from its
// creation, suspended 3 days later, and a link to a billing account in
// non-payment keeps the earlier block start.
func TestBillingUnlinkedOrganization(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	created := env.Clock.Now()
	org := env.NewUnlinkedOrg("bob")
	c := org.OwnerClient
	b := billingStatus(t, env.User(org.Member), org.ID)
	if b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || b.GetLinked() ||
		!b.GetBlockedSince().AsTime().Equal(created) || !b.GetSuspendTime().AsTime().Equal(created.Add(72*time.Hour)) {
		t.Fatalf("new organization status = %v", b)
	}
	g := &gateFixture{env: env, org: org, sandboxID: "01900000-0000-7000-8000-00000000beef"}
	for _, call := range g.blockedCalls() {
		if call.name == "Enroll" {
			continue // no enrollment token can exist
		}
		if got := billingReasonOf(t, call.name, call.call(ctx)); got != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED {
			t.Errorf("%s: reason %v", call.name, got)
		}
	}
	if _, err := c.Billing.GetBillingUsage(ctx, connect.NewRequest(&ownsandv1.GetBillingUsageRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("GetBillingUsage without a link: %v", err)
	}

	env.Clock.Advance(72 * time.Hour)
	if b := billingStatus(t, c, org.ID); b.GetPhase() != billing.PhaseSuspended || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("status after 3 days = %v", b)
	}
	_, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
	if got := billingReasonOf(t, "CreateEnrollmentToken", err); got != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("reason after 3 days = %v", got)
	}
	if err := env.App.Billing.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Logs.String(), `"source":"time"`) {
		t.Fatal("the time-based suspension was not logged")
	}

	// A link to a billing account that is also in non-payment keeps the
	// block start of the organization.
	fake.AddAccount(billingAccountB, "Unpaid", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN})
	fake.SetAccountStatus(billingAccountB, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, env.Clock.Now().Add(-time.Hour))
	resp, err := c.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: billingAccountB}))
	if err != nil {
		t.Fatal(err)
	}
	if b := resp.Msg.GetBilling(); b.GetPhase() != billing.PhaseSuspended || !b.GetLinked() || !b.GetBlockedSince().AsTime().Equal(created) {
		t.Fatalf("link in non-payment = %v", b)
	}
	if _, name, _ := fake.Link(org.ID); name != "org of bob" {
		t.Fatalf("organization_name sent = %q", name)
	}
	// Unlinking keeps the block start too.
	un, err := c.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if b := un.Msg.GetBilling(); b.GetPhase() != billing.PhaseSuspended || b.GetLinked() || !b.GetBlockedSince().AsTime().Equal(created) {
		t.Fatalf("unlink in non-payment = %v", b)
	}
	// A billing account in good standing recovers the organization.
	fake.AddAccount(billingAccountC, "Paid", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	resp, err = c.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: billingAccountC}))
	if err != nil || resp.Msg.GetBilling().GetPhase() != billing.PhaseAllowed {
		t.Fatalf("link in good standing = %v, %v", resp, err)
	}
	if _, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)})); err != nil {
		t.Fatalf("CreateEnrollmentToken after the link: %v", err)
	}
}

// TestBillingFailOpen checks that a linked organization whose cached status
// is more than 1 hour old is allowed by the RPC gates, with a warning, and
// that the fail open never resumes a billing-paused sandbox.
func TestBillingFailOpen(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	org := linkedOrg(t, env, fake, "carol", billingAccountA)
	c := org.OwnerClient
	_, _ = setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, env.Clock.Now().Add(-72*time.Hour))
	pollBilling(t, env)
	enforceBilling(t, env)
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	tokenReq := &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}
	if _, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(tokenReq)); billingReasonOf(t, "CreateEnrollmentToken", err) != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("suspended: %v", err)
	}

	// delino-accounts becomes unreachable; at 1 hour the cache is still
	// enforced, after it the gates fail open.
	fake.Fail(accountstest.MethodListBillingStatusChanges, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	env.Clock.Advance(time.Hour)
	if err := env.App.Billing.Poll(ctx); err == nil {
		t.Fatal("the poll succeeded while delino-accounts is down")
	}
	if b := billingStatus(t, c, org.ID); b.GetPhase() != billing.PhaseSuspended {
		t.Fatalf("status at 1 hour = %v", b)
	}
	env.Clock.Advance(time.Second)
	if b := billingStatus(t, c, org.ID); b.GetPhase() != billing.PhaseAllowed || !b.GetLinked() || b.GetBlockedSince() != nil {
		t.Fatalf("fail-open status = %v", b)
	}
	for range 2 {
		if _, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(tokenReq)); err != nil {
			t.Fatalf("CreateEnrollmentToken while failing open: %v", err)
		}
	}
	if n := strings.Count(env.Logs.String(), `"msg":"billing_status_stale"`); n != 1 {
		t.Fatalf("billing_status_stale logged %d times, want 1 (rate-limited)", n)
	}
	// The fail open never resumes the billing-paused sandbox.
	enforceBilling(t, env)
	if !pausedByBilling(t, env, sandboxID) {
		t.Fatal("the fail open cleared the billing mark")
	}
	var state int16
	if err := env.DB.Pool.QueryRow(ctx, "SELECT agent_state FROM sandboxes WHERE sandbox_id = $1", sandboxID).Scan(&state); err != nil || ownsandv1.SandboxState(state) != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("sandbox state = %v, %v", ownsandv1.SandboxState(state), err)
	}

	// A successful poll confirms the status again and restores the gates.
	fake.Fail(accountstest.MethodListBillingStatusChanges, nil)
	pollBilling(t, env)
	if _, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(tokenReq)); billingReasonOf(t, "CreateEnrollmentToken", err) != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("after a fresh poll: %v", err)
	}
}

// TestBillingOwnershipTransferAndDeletionUnlink checks that an ownership
// transfer and an organization deletion remove the billing link at once,
// and that an unlink that delino-accounts could not take is retried before
// the new owner's link.
func TestBillingOwnershipTransferAndDeletionUnlink(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	org := linkedOrg(t, env, fake, "dave", billingAccountA)
	fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	transferAt := env.Clock.Now()
	if _, err := org.OwnerClient.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: org.Admin})); err != nil {
		t.Fatal(err)
	}
	newOwner := env.User(org.Admin)
	b := billingStatus(t, newOwner, org.ID)
	if b.GetPhase() != billing.PhaseBlocked || b.GetLinked() || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || !b.GetBlockedSince().AsTime().Equal(transferAt) {
		t.Fatalf("status after the transfer = %v", b)
	}
	if _, err := newOwner.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)})); billingReasonOf(t, "CreateEnrollmentToken", err) != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED {
		t.Fatalf("after the transfer: %v", err)
	}
	if account, _, ok := fake.Link(org.ID); !ok || account != billingAccountA {
		t.Fatal("the fake lost the link although the unlink failed")
	}
	var pending int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks WHERE organization_id = $1", org.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending unlinks = %d, %v", pending, err)
	}
	// The former owner can no longer link.
	if _, err := org.OwnerClient.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: billingAccountA})); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("former owner link: %v", err)
	}
	// The new owner's link needs the pending unlink first.
	fake.AddAccount(billingAccountB, "New owner", map[string]accountsv1.BillingAccountRole{org.Admin: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	linkB := &ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: billingAccountB}
	if _, err := newOwner.Billing.LinkBillingAccount(ctx, connect.NewRequest(linkB)); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("link with a pending unlink while delino-accounts is down: %v", err)
	}
	fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	resp, err := newOwner.Billing.LinkBillingAccount(ctx, connect.NewRequest(linkB))
	if err != nil || resp.Msg.GetBilling().GetPhase() != billing.PhaseAllowed {
		t.Fatalf("new owner link = %v, %v", resp, err)
	}
	if account, _, _ := fake.Link(org.ID); account != billingAccountB {
		t.Fatalf("fake link = %q", account)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending unlinks after the link = %d, %v", pending, err)
	}

	// Deletion removes the link: at once when delino-accounts answers ...
	org2 := linkedOrg(t, env, fake, "erin", billingAccountC)
	if _, err := org2.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org2.ID})); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := fake.Link(org2.ID); ok {
		t.Fatal("the deleted organization is still linked")
	}
	// ... and through the retry when it does not.
	org3 := linkedOrg(t, env, fake, "frank", "01900000-0000-7000-8000-0000000000c4")
	fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	if _, err := org3.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org3.ID})); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := fake.Link(org3.ID); !ok {
		t.Fatal("the fake lost the link although the unlink failed")
	}
	fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	env.Clock.Advance(time.Minute)
	if err := env.App.Billing.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := fake.Link(org3.ID); ok {
		t.Fatal("the retried unlink did not reach delino-accounts")
	}
}

// TestBillingRPCs checks the BillingService answers and their errors.
func TestBillingRPCs(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	org := env.NewUnlinkedOrg("gina")
	c := org.OwnerClient
	fake.AddAccount(billingAccountA, "Owned", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	fake.AddAccount(billingAccountB, "Administered", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN})
	fake.AddAccount(billingAccountC, "Viewed", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER})

	list, err := c.Billing.ListLinkableBillingAccounts(ctx, connect.NewRequest(&ownsandv1.ListLinkableBillingAccountsRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	got := list.Msg.GetBillingAccounts()
	if len(got) != 2 || got[0].GetBillingAccountId() != billingAccountA || got[0].GetRole() != ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER ||
		got[1].GetBillingAccountId() != billingAccountB || got[1].GetRole() != ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN || got[1].GetName() != "Administered" {
		t.Fatalf("linkable accounts = %v", got)
	}
	page, err := c.Billing.ListLinkableBillingAccounts(ctx, connect.NewRequest(&ownsandv1.ListLinkableBillingAccountsRequest{OrganizationId: org.ID, PageSize: 1}))
	if err != nil || len(page.Msg.GetBillingAccounts()) != 1 || page.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}

	link := func(account string) error {
		_, err := c.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: account}))
		return err
	}
	for _, tc := range []struct {
		account string
		want    connect.Code
	}{
		{billingAccountC, connect.CodePermissionDenied},
		{"01900000-0000-7000-8000-00000000ffff", connect.CodeNotFound},
		{"not-a-uuid", connect.CodeInvalidArgument},
	} {
		if err := link(tc.account); testenv.Code(err) != tc.want {
			t.Fatalf("link %s: %v, want %v", tc.account, err, tc.want)
		}
	}
	if err := link(billingAccountA); err != nil {
		t.Fatal(err)
	}
	if err := link(billingAccountB); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("link to another account: %v", err)
	}
	if err := link(billingAccountA); err != nil {
		t.Fatalf("repeated link: %v", err)
	}

	// Usage of the current month, one entry per meter in order.
	window := billing.WindowStart(env.Clock.Now())
	if _, err := fake.Client().ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{
		{Service: billing.ServiceID, OrganizationId: org.ID, Meter: accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, WindowStart: timestamppb.New(window), Quantity: 10, AmountUsdMicros: 45},
	}})); err != nil {
		t.Fatal(err)
	}
	usage, err := env.User(org.Admin).Billing.GetBillingUsage(ctx, connect.NewRequest(&ownsandv1.GetBillingUsageRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	u := usage.Msg
	if u.GetBillingAccountId() != billingAccountA || u.GetBillingAccountName() != "Owned" || u.GetBalanceUsdMicros() != 10_000_000-45 || len(u.GetMeterUsages()) != 4 {
		t.Fatalf("usage = %v", u)
	}
	for i, m := range billing.Meters {
		mu := u.GetMeterUsages()[i]
		wantQ, wantA := int64(0), int64(0)
		if m == ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC {
			wantQ, wantA = 10, 45
		}
		if mu.GetMeter() != m || mu.GetQuantity() != wantQ || mu.GetAmountUsdMicros() != wantA {
			t.Fatalf("meter %d = %v", i, mu)
		}
	}

	// delino-accounts is down: the RPCs that need it are unavailable, the
	// status still answers from the cache.
	down := connect.NewError(connect.CodeInternal, errors.New("boom"))
	fake.Fail(accountstest.MethodGetOrganizationBilling, down)
	fake.Fail(accountstest.MethodListManageableBillingAccounts, down)
	fake.Fail(accountstest.MethodUnlinkOrganization, down)
	if _, err := c.Billing.GetBillingUsage(ctx, connect.NewRequest(&ownsandv1.GetBillingUsageRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("usage while down: %v", err)
	}
	if _, err := c.Billing.ListLinkableBillingAccounts(ctx, connect.NewRequest(&ownsandv1.ListLinkableBillingAccountsRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("list while down: %v", err)
	}
	if _, err := c.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("unlink while down: %v", err)
	}
	if b := billingStatus(t, c, org.ID); b.GetPhase() != billing.PhaseAllowed || !b.GetLinked() {
		t.Fatalf("status while down = %v", b)
	}
	fake.Fail(accountstest.MethodUnlinkOrganization, nil)

	// Unlink blocks at once and is idempotent.
	unlinkAt := env.Clock.Now()
	for range 2 {
		resp, err := c.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: org.ID}))
		if err != nil {
			t.Fatal(err)
		}
		if b := resp.Msg.GetBilling(); b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || !b.GetBlockedSince().AsTime().Equal(unlinkAt) {
			t.Fatalf("unlink = %v", b)
		}
	}
	if fake.Calls(accountstest.MethodUnlinkOrganization) != 2 { // one failed, one accepted; the repeated unlink sends nothing
		t.Fatalf("UnlinkOrganization calls = %d", fake.Calls(accountstest.MethodUnlinkOrganization))
	}
}

// TestBillingErrorDetailCodecs checks that a billing rejection carries the
// billing reason in the binary protobuf and in the Connect JSON encoding.
func TestBillingErrorDetailCodecs(t *testing.T) {
	env, _ := billingEnv(t)
	org := env.NewUnlinkedOrg("hank")
	req := &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}
	_, binErr := org.OwnerClient.Device.CreateEnrollmentToken(context.Background(), connect.NewRequest(req))
	_, jsonErr := env.JSONDeviceClient(testenv.UserToken(org.Owner)).CreateEnrollmentToken(context.Background(), connect.NewRequest(req))
	for name, err := range map[string]error{"binary": binErr, "json": jsonErr} {
		if got := billingReasonOf(t, name, err); got != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED {
			t.Fatalf("%s: reason = %v (err %v)", name, got, err)
		}
	}
}

// TestBillingLegacyWriterOrganization creates an organization the way the
// previous release (before billing) does, with plain inserts and no status
// row, while the new release serves requests (a rolling deployment). The
// migration's trigger gives the organization its unlinked status when the
// creation commits, so its owner can link a billing account with the normal
// gates, and a non-owner still cannot.
func TestBillingLegacyWriterOrganization(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	const owner, admin, orgID = "legacy-owner", "legacy-admin", "018f0000-0000-7000-8000-0000000000f1"
	created := env.Clock.Now().Add(-time.Hour)
	err := env.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, 'legacy org', $2)", orgID, created); err != nil {
			return err
		}
		for user, role := range map[string]ownsandv1.Role{owner: ownsandv1.Role_ROLE_OWNER, admin: ownsandv1.Role_ROLE_ADMIN} {
			membership, err := uuidv7.NewString()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, $3, $4, $5)",
				membership, orgID, user, int16(role), created); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM organization_billing WHERE organization_id = $1", orgID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("status rows of the legacy organization = %d, %v, want 1", rows, err)
	}

	ownerClient := env.User(owner)
	b := billingStatus(t, ownerClient, orgID)
	if b.GetPhase() != billing.PhaseBlocked || b.GetLinked() || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || !b.GetBlockedSince().AsTime().Equal(created) {
		t.Fatalf("status of the legacy organization = %v", b)
	}

	// A non-owner cannot link, and the owner can with the normal gates:
	// delino-accounts checks the billing account role.
	fake.AddAccount(billingAccountA, "Legacy account", map[string]accountsv1.BillingAccountRole{owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER, admin: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	link := &ownsandv1.LinkBillingAccountRequest{OrganizationId: orgID, BillingAccountId: billingAccountA}
	if _, err := env.User(admin).Billing.LinkBillingAccount(ctx, connect.NewRequest(link)); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("admin link of the legacy organization: %v", err)
	}
	resp, err := ownerClient.Billing.LinkBillingAccount(ctx, connect.NewRequest(link))
	if err != nil {
		t.Fatalf("owner link of the legacy organization: %v", err)
	}
	if resp.Msg.GetBilling().GetPhase() != billing.PhaseAllowed || !resp.Msg.GetBilling().GetLinked() || resp.Msg.GetBillingAccountName() != "Legacy account" {
		t.Fatalf("link response = %v", resp.Msg)
	}
	if account, _, ok := fake.Link(orgID); !ok || account != billingAccountA {
		t.Fatalf("fake link = %q, %v", account, ok)
	}
}

// linkAttemptFixture is an unlinked organization whose owner administers
// billingAccountA, on a fake delino-accounts that the test can make lose
// responses of the link call.
type linkAttemptFixture struct {
	env  *testenv.Env
	fake *accountstest.Server
	org  *testenv.Org
}

func newLinkAttemptFixture(t *testing.T, owner string) *linkAttemptFixture {
	t.Helper()
	env, fake := billingEnv(t)
	fake.AddAccount(billingAccountA, "Account of "+owner, map[string]accountsv1.BillingAccountRole{owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	return &linkAttemptFixture{env: env, fake: fake, org: env.NewUnlinkedOrg(owner)}
}

func (f *linkAttemptFixture) link(c *testenv.Clients, account string) error {
	_, err := c.Billing.LinkBillingAccount(context.Background(), connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: f.org.ID, BillingAccountId: account}))
	return err
}

func (f *linkAttemptFixture) unlink(c *testenv.Clients) error {
	_, err := c.Billing.UnlinkBillingAccount(context.Background(), connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: f.org.ID}))
	return err
}

// loseLinkResponse makes delino-accounts commit every later link and fail
// the response, and returns the error that the owner's link call gets.
func (f *linkAttemptFixture) loseLinkResponse(t *testing.T) {
	t.Helper()
	f.fake.FailReply(accountstest.MethodLinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("response lost")))
	if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("link with a lost response: %v", err)
	}
	f.fake.FailReply(accountstest.MethodLinkOrganization, nil)
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, false)
	if a, p := f.rows(t); a != 1 || p != 1 {
		t.Fatalf("link attempt rows = %d of %d rows, want 1 of 1", a, p)
	}
}

// rows returns the number of link attempt rows and of all rows of
// billing_pending_unlinks of the organization.
func (f *linkAttemptFixture) rows(t *testing.T) (attempts, all int) {
	t.Helper()
	if err := f.env.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE link_attempt_id IS NOT NULL), count(*)
		FROM billing_pending_unlinks WHERE organization_id = $1`, f.org.ID).Scan(&attempts, &all); err != nil {
		t.Fatal(err)
	}
	return attempts, all
}

func (f *linkAttemptFixture) wantRows(t *testing.T, attempts, all int) {
	t.Helper()
	if a, p := f.rows(t); a != attempts || p != all {
		t.Fatalf("link attempt rows = %d of %d rows, want %d of %d", a, p, attempts, all)
	}
}

func (f *linkAttemptFixture) wantRemoteLink(t *testing.T, want bool) {
	t.Helper()
	if _, _, ok := f.fake.Link(f.org.ID); ok != want {
		t.Fatalf("delino-accounts link = %v, want %v", ok, want)
	}
}

func (f *linkAttemptFixture) wantLocalLink(t *testing.T, want bool) {
	t.Helper()
	if b := billingStatus(t, f.org.OwnerClient, f.org.ID); b.GetLinked() != want {
		t.Fatalf("local link = %v, want %v (%v)", b.GetLinked(), want, b)
	}
}

// restart returns a status cache over the same database, like the one of a
// restarted instance; it has no memory of the attempt.
func (f *linkAttemptFixture) restart() *billing.Statuses {
	return billing.NewStatuses(billing.StatusOptions{DB: f.env.DB, Logger: testenv.Discard(), Now: f.env.Clock.Now, Client: f.fake.Client(),
		Timing: &billing.StatusTiming{Poll: time.Hour, Tick: time.Hour, UnlinkBackoffMin: time.Second, UnlinkBackoffMax: 5 * time.Minute}})
}

// TestBillingLinkLostResponseIsReconciled checks that a link whose response
// delino-accounts lost stays recorded across a restart, that the lease
// holder removes the unconfirmed remote link, and that a retry then links.
func TestBillingLinkLostResponseIsReconciled(t *testing.T) {
	f := newLinkAttemptFixture(t, "ivy")
	ctx := context.Background()
	f.loseLinkResponse(t)

	// The restarted instance reconciles the attempt: not before the first
	// back-off, then by removing the remote link.
	restarted := f.restart()
	if err := restarted.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, true)
	f.env.Clock.Advance(2 * time.Second)
	if err := restarted.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, false)
	f.wantRows(t, 0, 0)
	f.wantLocalLink(t, false)

	// The authorized retry recovers the link.
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, true)
	f.wantRows(t, 0, 0)
	if b := billingStatus(t, f.org.OwnerClient, f.org.ID); b.GetPhase() != billing.PhaseAllowed {
		t.Fatalf("status after the retry = %v", b)
	}
}

// TestBillingLinkLostResponseRetryRecovers checks that an authorized retry
// of the same account recovers the link at once, without the lease holder.
func TestBillingLinkLostResponseRetryRecovers(t *testing.T) {
	f := newLinkAttemptFixture(t, "jack")
	f.loseLinkResponse(t)
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatalf("retry: %v", err)
	}
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, true)
	f.wantRows(t, 0, 0)
	// The unconfirmed link was removed first, then linked again.
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
		t.Fatalf("UnlinkOrganization calls = %d, want 1", n)
	}
	// Nothing is left for the lease holder to remove.
	f.env.Clock.Advance(time.Hour)
	if err := f.restart().RetryPendingUnlinks(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, true)
}

// TestBillingLinkLostResponseThenRejectedRetry checks that a retry that
// delino-accounts rejects leaves neither a local link nor the unconfirmed
// remote link of the earlier attempt.
func TestBillingLinkLostResponseThenRejectedRetry(t *testing.T) {
	f := newLinkAttemptFixture(t, "kim")
	f.loseLinkResponse(t)
	// The owner is only a viewer of the billing account now.
	f.fake.AddAccount(billingAccountA, "Account of kim", map[string]accountsv1.BillingAccountRole{f.org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER})
	if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != connect.CodePermissionDenied && testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("rejected retry: %v", err)
	}
	f.wantRemoteLink(t, false)
	f.wantLocalLink(t, false)
	f.wantRows(t, 0, 0)
}

// TestBillingUnlinkRemovesUnconfirmedLink checks that an explicit unlink
// removes the remote link of an attempt that has no cached account ID, and
// stays idempotent without a record.
func TestBillingUnlinkRemovesUnconfirmedLink(t *testing.T) {
	f := newLinkAttemptFixture(t, "lena")
	f.loseLinkResponse(t)
	before := f.fake.Calls(accountstest.MethodUnlinkOrganization)
	if err := f.unlink(f.org.OwnerClient); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, false)
	f.wantRows(t, 0, 0)
	f.wantLocalLink(t, false)
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != before+1 {
		t.Fatalf("UnlinkOrganization calls = %d, want %d", n, before+1)
	}
	// A second unlink has nothing to remove and sends nothing.
	if err := f.unlink(f.org.OwnerClient); err != nil {
		t.Fatal(err)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != before+1 {
		t.Fatalf("UnlinkOrganization calls after a repeated unlink = %d, want %d", n, before+1)
	}
	// While delino-accounts is down, the unlink fails and the record stays.
	f2 := newLinkAttemptFixture(t, "lena2")
	f2.loseLinkResponse(t)
	f2.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	if err := f2.unlink(f2.org.OwnerClient); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("unlink while delino-accounts is down: %v", err)
	}
	f2.wantRows(t, 1, 1)
	f2.wantRemoteLink(t, true)
}

// TestBillingTransferAndDeletionRemoveUnconfirmedLink checks that an
// ownership transfer and an organization deletion remove the remote link of
// an attempt that has no cached account ID.
func TestBillingTransferAndDeletionRemoveUnconfirmedLink(t *testing.T) {
	ctx := context.Background()
	f := newLinkAttemptFixture(t, "mona")
	f.loseLinkResponse(t)
	if _, err := f.org.OwnerClient.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: f.org.ID, UserId: f.org.Admin})); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, false)
	f.wantRows(t, 0, 0)

	g := newLinkAttemptFixture(t, "nora")
	g.loseLinkResponse(t)
	if _, err := g.org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: g.org.ID})); err != nil {
		t.Fatal(err)
	}
	g.wantRemoteLink(t, false)
	g.wantRows(t, 0, 0)

	// A deletion while delino-accounts is down keeps the record, which the
	// lease holder works off later; the organization row is gone.
	h := newLinkAttemptFixture(t, "olga")
	h.loseLinkResponse(t)
	h.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	if _, err := h.org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: h.org.ID})); err != nil {
		t.Fatal(err)
	}
	h.wantRemoteLink(t, true)
	h.wantRows(t, 0, 1)
	h.fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	h.env.Clock.Advance(time.Hour)
	if err := h.restart().RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantRemoteLink(t, false)
	h.wantRows(t, 0, 0)
}

// TestBillingLinkLocalCommitFailure checks that a link that delino-accounts
// accepted but that the local transaction could not record stays available
// for reconciliation, and that a retry recovers.
func TestBillingLinkLocalCommitFailure(t *testing.T) {
	f := newLinkAttemptFixture(t, "pia")
	ctx := context.Background()
	// A trigger fails the local write of the link, as a failed local commit
	// would.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_billing_link() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected local failure'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_billing_link BEFORE UPDATE ON organization_billing FOR EACH ROW WHEN (NEW.billing_account_id IS NOT NULL) EXECUTE FUNCTION fail_billing_link()`,
	} {
		if _, err := f.env.DB.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != connect.CodeInternal {
		t.Fatalf("link with a failing local write: %v", err)
	}
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, false)
	f.wantRows(t, 1, 1)

	// After a restart the lease holder removes the unconfirmed link.
	f.env.Clock.Advance(2 * time.Second)
	if err := f.restart().RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, false)
	f.wantRows(t, 0, 0)

	// A retry recovers once the local write works.
	if _, err := f.env.DB.Pool.Exec(ctx, "DROP TRIGGER fail_billing_link ON organization_billing"); err != nil {
		t.Fatal(err)
	}
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, true)
	f.wantRows(t, 0, 0)
}

// TestBillingLinkDefinitiveFailureLeavesNothing checks that a link that
// delino-accounts rejects leaves no record and sends no unlink.
func TestBillingLinkDefinitiveFailureLeavesNothing(t *testing.T) {
	f := newLinkAttemptFixture(t, "quin")
	f.fake.AddAccount(billingAccountC, "Viewed", map[string]accountsv1.BillingAccountRole{f.org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER})
	for _, tc := range []struct {
		account string
		want    connect.Code
	}{
		{billingAccountC, connect.CodePermissionDenied},
		{"01900000-0000-7000-8000-00000000ffff", connect.CodeNotFound},
	} {
		if err := f.link(f.org.OwnerClient, tc.account); testenv.Code(err) != tc.want {
			t.Fatalf("link %s: %v, want %v", tc.account, err, tc.want)
		}
		f.wantRows(t, 0, 0)
		f.wantLocalLink(t, false)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 0 {
		t.Fatalf("UnlinkOrganization calls = %d, want none", n)
	}
	// The record of a rejected repeat link of the linked account does not
	// remove the working link.
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.fake.AddAccount(billingAccountA, "Account of quin", map[string]accountsv1.BillingAccountRole{f.org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER})
	if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != connect.CodePermissionDenied && testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("rejected repeat link: %v", err)
	}
	f.wantRemoteLink(t, true)
	f.wantLocalLink(t, true)
	f.wantRows(t, 0, 0)
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 0 {
		t.Fatalf("UnlinkOrganization calls = %d, want none", n)
	}
}

// TestBillingLinkStaleCompletionDoesNothing checks that the completion of an
// old link attempt cannot record a link after a newer authorized operation
// took the attempt over, and that no unlink overtakes the running call.
func TestBillingLinkStaleCompletionDoesNothing(t *testing.T) {
	f := newLinkAttemptFixture(t, "rosa")
	ctx := context.Background()
	gate := f.fake.Hold(accountstest.MethodLinkOrganization)
	done := make(chan error, 1)
	go func() { done <- f.link(f.org.OwnerClient, billingAccountA) }()
	<-gate.Entered
	f.wantRows(t, 1, 1)

	// While the call runs, other link operations wait for it.
	if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("second link while the first runs: %v", err)
	}
	if err := f.unlink(f.org.OwnerClient); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("unlink while the link runs: %v", err)
	}
	f.wantRows(t, 1, 1)

	// An ownership transfer is not refused: it takes the attempt over, and
	// its unlink waits for the running call.
	if _, err := f.org.OwnerClient.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: f.org.ID, UserId: f.org.Admin})); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 0, 1)
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 0 {
		t.Fatalf("an unlink overtook the running link call: %d calls", n)
	}

	// The old call lands and its completion records nothing.
	gate.Release()
	if err := <-done; testenv.Code(err) != connect.CodeAborted {
		t.Fatalf("stale completion: %v, want aborted", err)
	}
	f.wantLocalLink(t, false)
	f.wantRemoteLink(t, true)
	if err := f.env.App.Billing.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, true) // not due before the call deadline

	// After the deadline, the pending unlink removes the link that the old
	// call created.
	f.env.Clock.Advance(time.Minute)
	if err := f.env.App.Billing.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.wantRemoteLink(t, false)
	f.wantRows(t, 0, 0)
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
		t.Fatalf("UnlinkOrganization calls = %d, want 1", n)
	}

	// The new owner links afterwards.
	newOwner := f.env.User(f.org.Admin)
	f.fake.AddAccount(billingAccountB, "New owner", map[string]accountsv1.BillingAccountRole{f.org.Admin: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	if err := f.link(newOwner, billingAccountB); err != nil {
		t.Fatal(err)
	}
	if account, _, ok := f.fake.Link(f.org.ID); !ok || account != billingAccountB {
		t.Fatalf("remote link after the new owner's link = %q, %v", account, ok)
	}
}
