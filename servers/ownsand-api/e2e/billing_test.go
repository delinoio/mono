// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsand "github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// billingReason returns the billing reason of err and checks the error
// shape: resource_exhausted and an ErrorDetail whose message links to
// https://accounts.delino.io.
func billingReason(t *testing.T, err error) ownsandv1.BillingReason {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("want a billing error, got %v", err)
	}
	for _, d := range ce.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		if detail, ok := v.(*ownsandv1.ErrorDetail); ok {
			if !strings.Contains(detail.GetMessage(), "https://accounts.delino.io") || detail.GetRetryAfter() != nil {
				t.Fatalf("billing detail = %v", detail)
			}
			return detail.GetBillingReason()
		}
	}
	t.Fatalf("billing error without an ErrorDetail: %v", err)
	return 0
}

// hasLogMessage reports whether the process name logged a JSON record with
// the message msg.
func (l *logSink) hasLogMessage(name, msg string) bool {
	prefix := name + ": "
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		text, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var event struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(text), &event) == nil && event.Msg == msg {
			return true
		}
	}
	return false
}

const (
	// logWaitBudget bounds how long requireLogMessages waits for the log
	// records of a child process. The scanner goroutine of the logSink stores
	// a record some time after the process wrote it, so a test that checks
	// right after its functional checks must wait for the record.
	logWaitBudget = 10 * time.Second
	// logWaitInterval is the polling interval of waitForLogMessages.
	logWaitInterval = 25 * time.Millisecond
)

// waitForLogMessages polls until the process name logged a JSON record for
// every message in msgs, for at most budget or until ctx ends. It returns the
// messages that are still missing, in the order of msgs; the result is empty
// when all of them arrived.
func (l *logSink) waitForLogMessages(ctx context.Context, name string, budget time.Duration, msgs ...string) []string {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(logWaitInterval)
	defer ticker.Stop()
	for {
		var missing []string
		for _, msg := range msgs {
			if !l.hasLogMessage(name, msg) {
				missing = append(missing, msg)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			// One last check: a record may have arrived with the end.
			missing = missing[:0]
			for _, msg := range msgs {
				if !l.hasLogMessage(name, msg) {
					missing = append(missing, msg)
				}
			}
			return missing
		case <-ticker.C:
		}
	}
}

// requireLogMessages fails the test when the process name did not log every
// message in msgs within logWaitBudget of the test context ctx. The failure
// names the missing messages.
func (l *logSink) requireLogMessages(ctx context.Context, t *testing.T, name string, msgs ...string) {
	t.Helper()
	if missing := l.waitForLogMessages(ctx, name, logWaitBudget, msgs...); len(missing) > 0 {
		t.Fatalf("the %s process did not log %q within %s", name, missing, logWaitBudget)
	}
}

// TestBillingBlockSuspendRecover runs the billing phases against the real
// control plane and agent with a fake delino-accounts: a block rejects
// resource creation, a suspension pauses the running sandbox and rejects the
// data path, the recovery resumes it, and an unlink blocks with
// ACCOUNT_REQUIRED.
func TestBillingBlockSuspendRecover(t *testing.T) {
	s := newStack(t)
	ctx := s.ctx(5 * time.Minute)
	sb, err := s.apiKeyClient(ownsand.TransportRelay).CreateSandbox(ctx, createParams())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer sb.Close()
	if info, err := sb.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = (%v, %v)", info.GetState(), err)
	}
	billingSvc := s.billingClient()
	status := func() *ownsandv1.OrganizationBilling {
		t.Helper()
		resp, err := billingSvc.GetBillingStatus(ctx, connect.NewRequest(&ownsandv1.GetBillingStatusRequest{OrganizationId: s.orgID}))
		if err != nil {
			t.Fatalf("GetBillingStatus: %v", err)
		}
		return resp.Msg.GetBilling()
	}
	// Raw clients: the SDK treats billing errors in a later layer.
	auth := connect.WithInterceptors(bearerToken(func() string { return s.apiKey }))
	devices := ownsandv1connect.NewDeviceServiceClient(http.DefaultClient, s.apiURL, connect.WithInterceptors(bearerToken(s.userToken)))
	sandboxes := ownsandv1connect.NewSandboxServiceClient(http.DefaultClient, s.apiURL, auth)
	dataPath := ownsandv1connect.NewDataPathServiceClient(http.DefaultClient, s.apiURL, auth)
	createToken := func() error {
		_, err := devices.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: s.orgID, Ttl: durationpb.New(time.Hour)}))
		return err
	}
	authorizeExec := func() error {
		execID, _ := uuidv7.NewString()
		authID, _ := uuidv7.NewString()
		salt, _ := relaybinding.NewSalt()
		req := &ownsandv1.ExecRequest{OrganizationId: s.orgID, SandboxId: sb.ID(), ExecId: execID, Command: []string{"/bin/true"}, BindingSalt: salt}
		_, err := dataPath.AuthorizeRelay(ctx, connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{OrganizationId: s.orgID, SandboxId: sb.ID(),
			OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationId: execID, RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}))
		return err
	}
	waitState := func(want ownsandv1.SandboxState) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			info, err := sb.Get(ctx)
			if err == nil && info.GetState() == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("sandbox did not reach %v: last %v, %v", want, info.GetState(), err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// BLOCKED: the billing account has no credit. A repeated link applies
	// the status at once instead of waiting for the 1-minute poll.
	s.accounts.SetAccountStatus(s.billingAccountID, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, time.Now())
	if b := s.linkBilling(ctx); b.GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_BLOCKED || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_BLOCKED {
		t.Fatalf("blocked status = %v", b)
	}
	if got := billingReason(t, createToken()); got != ownsandv1.BillingReason_BILLING_REASON_BLOCKED {
		t.Fatalf("CreateEnrollmentToken reason = %v", got)
	}
	_, err = sandboxes.CreateSandbox(ctx, connect.NewRequest(&ownsandv1.CreateSandboxRequest{OrganizationId: s.orgID, IdempotencyKey: s.orgID,
		Source: &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}}}))
	if got := billingReason(t, err); got != ownsandv1.BillingReason_BILLING_REASON_BLOCKED {
		t.Fatalf("CreateSandbox reason = %v", got)
	}
	// Exec stays allowed while blocked.
	if out, _, res := execCollect(ctx, t, sb, "/bin/echo", "blocked"); out != "blocked\n" || exitCode(res) != 0 {
		t.Fatalf("exec while blocked = (%q, %+v)", out, res)
	}

	// SUSPENDED: the suspension worker pauses the running sandbox, and the
	// data path is rejected.
	s.accounts.SetAccountStatus(s.billingAccountID, accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, time.Now().Add(-72*time.Hour))
	if b := s.linkBilling(ctx); b.GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED {
		t.Fatalf("suspended status = %v", b)
	}
	waitState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if got := billingReason(t, authorizeExec()); got != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("AuthorizeRelay reason = %v", got)
	}
	_, err = sandboxes.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: s.orgID, SandboxId: sb.ID()}))
	if got := billingReason(t, err); got != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("ResumeSandbox reason = %v", got)
	}

	// Recovery: the billing-paused sandbox resumes by itself.
	s.accounts.SetAccountStatus(s.billingAccountID, accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, time.Time{})
	if b := s.linkBilling(ctx); b.GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED {
		t.Fatalf("recovered status = %v", b)
	}
	waitState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if out, _, res := execCollect(ctx, t, sb, "/bin/echo", "recovered"); out != "recovered\n" || exitCode(res) != 0 {
		t.Fatalf("exec after the recovery = (%q, %+v)", out, res)
	}
	if err := createToken(); err != nil {
		t.Fatalf("CreateEnrollmentToken after the recovery: %v", err)
	}

	// Unlink: blocked with ACCOUNT_REQUIRED at once.
	if _, err := billingSvc.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: s.orgID})); err != nil {
		t.Fatalf("UnlinkBillingAccount: %v", err)
	}
	if b := status(); b.GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_BLOCKED || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || b.GetLinked() {
		t.Fatalf("unlinked status = %v", b)
	}
	if got := billingReason(t, createToken()); got != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED {
		t.Fatalf("CreateEnrollmentToken reason after the unlink = %v", got)
	}
	if _, _, linked := s.accounts.Link(s.orgID); linked {
		t.Fatal("the fake delino-accounts still has the link")
	}
	// The log scanner stores the records asynchronously, so wait for them.
	s.requireAPILogMessages(ctx, t, "billing_sandbox_paused", "billing_sandbox_resumed")
}
