// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// TestTypeScriptSDKRelay runs packages/ownsand-sdk/src/e2e.test.ts against
// the real control plane and agent over the relay. It needs the workspace
// dependencies (`pnpm install`) and is skipped when pnpm is not on PATH.
func TestTypeScriptSDKRelay(t *testing.T) {
	requireE2E(t)
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm is not on PATH; skipping the TypeScript SDK end-to-end test")
	}
	s := newStack(t)
	control := s.billingControl()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpm, "exec", "rstest", "run", "src/e2e.test.ts")
	cmd.Dir = filepath.Join(moduleRoot(t), "packages", "ownsand-sdk")
	cmd.Env = append(os.Environ(),
		"OWNSAND_E2E_API_URL="+s.apiURL,
		"OWNSAND_E2E_API_KEY="+s.apiKey,
		"OWNSAND_E2E_ORG="+s.orgID,
		"OWNSAND_E2E_BILLING_URL="+control.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TypeScript SDK end-to-end test: %v\n%s", err, out)
	}
	t.Logf("TypeScript SDK end-to-end test:\n%s", out)
	// The log scanner stores the records asynchronously, so wait for them.
	s.logs.requireLogMessages(ctx, t, "api", "billing_sandbox_paused", "billing_sandbox_resumed")
}

// billingControlPath is the path prefix of the billing control endpoint.
const billingControlPath = "/billing/"

// billingControlTarget is a billing status that the billing control endpoint
// applies.
type billingControlTarget struct {
	// status is the status of the billing account in the fake
	// delino-accounts.
	status accountsv1.BillingStatus
	// blockedFor is how long ago the non-payment started; zero for ALLOWED.
	// A suspension starts 3 days after the block.
	blockedFor time.Duration
	// phase is the billing phase that the control plane must apply.
	phase ownsandv1.BillingPhase
}

// billingControlTargets maps the last path segment of a billing control
// request to the status that it applies. The names match the
// BillingControlStatus enum of packages/ownsand-sdk/src/e2e.test.ts.
var billingControlTargets = map[string]billingControlTarget{
	"suspended": {accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, 72 * time.Hour, ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED},
	"allowed":   {accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, 0, ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED},
}

// billingControl serves the billing control endpoint of the TypeScript SDK
// end-to-end test on loopback. `POST /billing/<status>` sets the status of
// the suite's billing account in the fake delino-accounts and applies it at
// once with a repeated link of the user, as linkBilling does; the API key
// of the TypeScript test cannot link. It answers 200 when the control plane
// applied the billing phase of the status.
func (s *stack) billingControl() *httptest.Server {
	s.t.Helper()
	// The token is signed here because the handler goroutines must not call
	// t.Fatal; it is valid for an hour, longer than the test.
	tok := s.userToken()
	billingSvc := ownsandv1connect.NewBillingServiceClient(http.DefaultClient, s.apiURL,
		connect.WithInterceptors(bearerToken(func() string { return tok })))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, billingControlPath)
		target, known := billingControlTargets[name]
		if r.Method != http.MethodPost || !ok || !known {
			http.Error(w, "unknown billing control request", http.StatusNotFound)
			return
		}
		var blockedSince time.Time
		if target.blockedFor > 0 {
			blockedSince = time.Now().Add(-target.blockedFor)
		}
		s.accounts.SetAccountStatus(s.billingAccountID, target.status, blockedSince)
		resp, err := billingSvc.LinkBillingAccount(r.Context(), connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: s.orgID, BillingAccountId: s.billingAccountID}))
		if err != nil {
			http.Error(w, "LinkBillingAccount: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if got := resp.Msg.GetBilling().GetPhase(); got != target.phase {
			http.Error(w, fmt.Sprintf("billing phase = %v, want %v", got, target.phase), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, target.phase)
	}))
	s.t.Cleanup(srv.Close)
	return srv
}
