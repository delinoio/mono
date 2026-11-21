// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// fakeBilling serves BillingService for the wrapper tests.
type fakeBilling struct {
	ownsandv1connect.UnimplementedBillingServiceHandler
	cp *fakeCP
}

func (f *fakeBilling) GetBillingStatus(_ context.Context, req *connect.Request[ownsandv1.GetBillingStatusRequest]) (*connect.Response[ownsandv1.GetBillingStatusResponse], error) {
	if req.Msg.GetOrganizationId() != f.cp.orgID {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	}
	return connect.NewResponse(&ownsandv1.GetBillingStatusResponse{Billing: &ownsandv1.OrganizationBilling{
		Phase:  ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED,
		Linked: true,
	}}), nil
}

func (f *fakeBilling) GetBillingUsage(_ context.Context, req *connect.Request[ownsandv1.GetBillingUsageRequest]) (*connect.Response[ownsandv1.GetBillingUsageResponse], error) {
	if req.Msg.GetOrganizationId() != f.cp.orgID {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	}
	return connect.NewResponse(&ownsandv1.GetBillingUsageResponse{BillingAccountName: "Acme", BalanceUsdMicros: -1}), nil
}

func TestBillingServiceFillsOrganization(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	st, err := c.Billing().GetStatus(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.GetBilling().GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED || !st.GetBilling().GetLinked() {
		t.Fatalf("status %v", st)
	}
	usage, err := c.Billing().GetUsage(context.Background(), &ownsandv1.GetBillingUsageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if usage.GetBillingAccountName() != "Acme" || usage.GetBalanceUsdMicros() != -1 {
		t.Fatalf("usage %v", usage)
	}
	if _, err := c.Billing().GetStatus(context.Background(), &ownsandv1.GetBillingStatusRequest{OrganizationId: mustID(t)}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an explicit organization must override the default, got %v", err)
	}
}

var billingReasons = []ownsandv1.BillingReason{
	ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED,
	ownsandv1.BillingReason_BILLING_REASON_BLOCKED,
	ownsandv1.BillingReason_BILLING_REASON_SUSPENDED,
}

const billingMessage = "the organization is blocked for billing; manage billing at https://accounts.delino.io"

// billingErr returns a billing error as billing enforcement of the control
// plane sends it: resource_exhausted with an ErrorDetail that carries the
// reason and no retry_after.
func billingErr(reason ownsandv1.BillingReason) error {
	return detailErr(connect.CodeResourceExhausted, billingMessage, &ownsandv1.ErrorDetail{
		Code:          ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
		Message:       billingMessage,
		BillingReason: reason,
	})
}

func checkBillingErr(t *testing.T, err error, reason ownsandv1.BillingReason, wantDataPath bool) {
	t.Helper()
	if err == nil {
		t.Fatal("want a billing error, got nil")
	}
	if got := BillingReasonOf(err); got != reason {
		t.Fatalf("BillingReasonOf = %v, want %v (err %v)", got, reason, err)
	}
	if !IsBillingError(err) {
		t.Fatalf("IsBillingError(%v) = false", err)
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message(), "https://accounts.delino.io") {
		t.Fatalf("the server message with the billing link must reach the caller, got %v", err)
	}
	var dp *DataPathError
	if wantDataPath != errors.As(err, &dp) {
		t.Fatalf("DataPathError = %v, want %v (err %v)", errors.As(err, &dp), wantDataPath, err)
	}
}

func TestBillingReasonOf(t *testing.T) {
	for _, reason := range billingReasons {
		err := billingErr(reason)
		if got := BillingReasonOf(err); got != reason {
			t.Fatalf("BillingReasonOf = %v, want %v", got, reason)
		}
		wrapped := &DataPathError{Operation: "exec", OperationID: "x", Err: err}
		if got := BillingReasonOf(wrapped); got != reason || !IsBillingError(wrapped) {
			t.Fatalf("wrapped: BillingReasonOf = %v, want %v", got, reason)
		}
		if got := BillingReasonOf(errorFromDetail(&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, BillingReason: reason})); got != reason {
			t.Fatalf("frame error: BillingReasonOf = %v, want %v", got, reason)
		}
	}
	notBilling := map[string]error{
		"nil":                     nil,
		"plain error":             errors.New("x"),
		"capacity":                connect.NewError(connect.CodeResourceExhausted, errors.New("agent at capacity")),
		"rate limit":              detailErr(connect.CodeResourceExhausted, "slow down", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, RetryAfter: durationpb.New(time.Second)}),
		"other code with reason":  detailErr(connect.CodePermissionDenied, "no", &ownsandv1.ErrorDetail{BillingReason: ownsandv1.BillingReason_BILLING_REASON_BLOCKED}),
		"unspecified reason only": detailErr(connect.CodeResourceExhausted, "x", &ownsandv1.ErrorDetail{}),
	}
	for name, err := range notBilling {
		if BillingReasonOf(err) != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED || IsBillingError(err) {
			t.Fatalf("%s: reported as a billing error", name)
		}
	}
}

func TestCreateSandboxBillingErrorIsNotRetried(t *testing.T) {
	for _, reason := range billingReasons {
		t.Run(reason.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.mu.Lock()
			cp.createErr = func(int) error { return billingErr(reason) }
			cp.mu.Unlock()
			c := cp.client(t, TransportRelay)
			_, err := c.CreateSandbox(context.Background(), validParams())
			checkBillingErr(t, err, reason, false)
			cp.mu.Lock()
			calls := cp.createCalls
			cp.mu.Unlock()
			if calls != 1 {
				t.Fatalf("%d CreateSandbox attempts, want 1", calls)
			}
		})
	}
}

func TestCreateSandboxRateLimitIsStillRetried(t *testing.T) {
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.createErr = func(n int) error {
		if n == 1 {
			return detailErr(connect.CodeResourceExhausted, "rate limited", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, RetryAfter: durationpb.New(50 * time.Millisecond)})
		}
		return nil
	}
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	if _, err := c.CreateSandbox(context.Background(), validParams()); err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	calls := cp.createCalls
	cp.mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d CreateSandbox attempts, want 2", calls)
	}
}

func TestAuthorizeRelayBillingErrorIsNotRetried(t *testing.T) {
	for _, reason := range billingReasons {
		t.Run(reason.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(1, 0)
			cp.authScript = func(int, *ownsandv1.AuthorizeRelayRequest) authAction {
				return authAction{err: billingErr(reason)}
			}
			s := cp.handle(t, cp.client(t, TransportRelay))
			_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
			checkBillingErr(t, err, reason, true)
			if n := len(cp.authRequests()); n != 1 {
				t.Fatalf("%d AuthorizeRelay attempts, want 1", n)
			}
			if n := cp.relayCount("Exec") + cp.relayCount("AttachExec"); n != 0 {
				t.Fatalf("%d relay streams after a billing error", n)
			}
		})
	}
}

func TestRelayRPCBillingErrorIsNotRetried(t *testing.T) {
	reason := ownsandv1.BillingReason_BILLING_REASON_SUSPENDED
	ops := map[string]struct {
		methods []string
		run     func(s *Sandbox) error
	}{
		"exec": {[]string{"Exec", "AttachExec"}, func(s *Sandbox) error {
			_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
			return err
		}},
		"read file": {[]string{"ReadFile"}, func(s *Sandbox) error {
			_, err := s.ReadFileBytes(context.Background(), "/r")
			return err
		}},
		"write file": {[]string{"WriteFile"}, func(s *Sandbox) error {
			return s.WriteFileBytes(context.Background(), "/w", []byte("data"), WriteOptions{})
		}},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(1, 0)
			cp.agent.setFile("/r", []byte("content"))
			cp.relayScript = func(string, int) relayAction { return relayAction{err: billingErr(reason)} }
			s := cp.handle(t, cp.client(t, TransportRelay))
			checkBillingErr(t, op.run(s), reason, true)
			calls := 0
			for _, m := range op.methods {
				calls += cp.relayCount(m)
			}
			if calls != 1 {
				t.Fatalf("%d relay calls, want 1", calls)
			}
			if n := len(cp.authRequests()); n != 1 {
				t.Fatalf("%d AuthorizeRelay calls, want 1 (no renewal for a retry)", n)
			}
		})
	}
}

// A billing error inside relay recovery ends the operation at once, both from
// the AuthorizeRelay renewal and from the continuation stream.
func TestBillingErrorInRelayRecoveryIsNotRetried(t *testing.T) {
	reason := ownsandv1.BillingReason_BILLING_REASON_SUSPENDED
	t.Run("authorize renewal", func(t *testing.T) {
		cp := newFakeCP(t)
		cp.agent.run = lineRun(3, 20*time.Millisecond)
		cp.relayScript = func(_ string, n int) relayAction {
			if n == 1 {
				return relayAction{abortAfter: 1}
			}
			return relayAction{}
		}
		cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
			if n >= 2 {
				return authAction{err: billingErr(reason)}
			}
			return authAction{}
		}
		s := cp.handle(t, cp.client(t, TransportRelay))
		_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
		checkBillingErr(t, err, reason, true)
		if n := len(cp.authRequests()); n != 2 {
			t.Fatalf("%d AuthorizeRelay calls, want 2 (the first grant and one renewal)", n)
		}
		if n := cp.relayCount("Exec") + cp.relayCount("AttachExec"); n != 1 {
			t.Fatalf("%d relay streams, want 1", n)
		}
	})
	t.Run("continuation", func(t *testing.T) {
		cp := newFakeCP(t)
		cp.agent.run = lineRun(3, 20*time.Millisecond)
		cp.relayScript = func(method string, _ int) relayAction {
			if method == "Exec" {
				return relayAction{abortAfter: 1}
			}
			return relayAction{err: billingErr(reason)}
		}
		s := cp.handle(t, cp.client(t, TransportRelay))
		_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
		checkBillingErr(t, err, reason, true)
		if n := cp.relayCount("Exec") + cp.relayCount("AttachExec"); n != 2 {
			t.Fatalf("%d relay streams, want 2 (the interrupted stream and one continuation)", n)
		}
	})
}

func TestOpenSessionBillingErrorEndsOperation(t *testing.T) {
	for _, tr := range []Transport{TransportAuto, TransportPeerToPeer} {
		t.Run(tr.String(), func(t *testing.T) {
			reason := ownsandv1.BillingReason_BILLING_REASON_SUSPENDED
			cp := newFakeCP(t)
			cp.agent.run = lineRun(1, 0)
			cp.mu.Lock()
			cp.openErr = billingErr(reason)
			cp.mu.Unlock()
			s := cp.handle(t, cp.client(t, tr))
			_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
			checkBillingErr(t, err, reason, true)
			if n := cp.openSessionCount(); n != 1 {
				t.Fatalf("%d OpenSession calls, want 1", n)
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls; a billing error must not fall back to the relay", n)
			}
		})
	}
}
