// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
)

// nullChargeFixture is an outcomeFixture whose Paddle charge answers with a
// success status and a null entity instead of hanging. The billing account
// lock is still held by another connection while the charge is sent, so the
// outcome write waits for it, as in the timeout tests.
func nullChargeFixture(t *testing.T, finalize string) (*fixture, string) {
	t.Helper()
	f, acct, _ := outcomeFixture(t, outcomeTestLockHold, outcomeTestOutcomeBudget, finalize)
	f.paddle.FailCharges(nil)
	f.paddle.Respond("POST /subscriptions", http.StatusOK, `{"data":null}`)
	return f, acct
}

// A charge answer without an entity is not a confirmed charge. The top-up
// moves to unknown, the caller gets the provider-unavailable answer, and a
// retry with the same key sends no second charge.
func TestManualTopUpRecordsUnknownForANullChargeEntity(t *testing.T) {
	f, acct := nullChargeFixture(t, "")

	err := f.manualTopUp(acct, topUpKey)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("CreateTopUp error = %v, want unavailable", err)
	}
	wantStatuses(t, "after the null answer", f.topUpStatuses(acct), "unknown")
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}

	err = f.manualTopUp(acct, topUpKey)
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("retry error = %v, want unavailable", err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges after the retry = %d, want 1", n)
	}
	f.wantCredit(acct, 0, 0)
}

// A webhook that finalized the top-up while the outcome write waited keeps
// its result, as with a timeout.
func TestNullChargeEntityKeepsAFinalizedTopUp(t *testing.T) {
	f, acct := nullChargeFixture(t, "completed")

	err := f.manualTopUp(acct, topUpKey)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("CreateTopUp error = %v, want unavailable", err)
	}
	wantStatuses(t, "after the finalization", f.topUpStatuses(acct), "completed")
}
