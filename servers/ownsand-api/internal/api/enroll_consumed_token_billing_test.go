// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// requireInvalidEnrollmentToken checks that err is the unauthenticated
// "invalid enrollment token" error, which carries no billing detail.
func requireInvalidEnrollmentToken(t *testing.T, step string, err error) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Message() != "invalid enrollment token" {
		t.Fatalf("%s: want unauthenticated invalid enrollment token, got %v", step, err)
	}
	if reason := errDetail(err).GetBillingReason(); reason != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
		t.Fatalf("%s: the rejection reveals billing reason %v", step, reason)
	}
}

// TestEnrollConsumedTokenIsValidatedBeforeBilling checks the order of the
// Enroll checks: a consumed token that the caller cannot use (another public
// key, or no recorded device) is rejected as invalid in every billing phase,
// an eligible retry sees the billing result, and a first enrollment is gated
// by billing without consuming the token.
func TestEnrollConsumedTokenIsValidatedBeforeBilling(t *testing.T) {
	env, fake := billingEnv(t)
	ctx := context.Background()
	org := linkedOrg(t, env, fake, "alice", billingAccountA)
	agent := env.Clients("").Agent

	newToken := func() string {
		resp, err := org.OwnerClient.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetEnrollmentToken()
	}
	newKey := func() ed25519.PublicKey {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return pub
	}
	enroll := func(token string, pub ed25519.PublicKey) (*ownsandv1.EnrollResponse, error) {
		resp, err := agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
			EnrollmentToken: token, PublicKey: pub, Name: "host", HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0",
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	deviceCount := func() int {
		var n int
		if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM devices WHERE organization_id = $1", org.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// tokenRow reads what Enroll may change on a token.
	type tokenRow struct {
		used   bool
		device string
		key    string
	}
	tokenState := func(token string) tokenRow {
		var r tokenRow
		err := env.DB.Pool.QueryRow(ctx, `SELECT use_time IS NOT NULL, COALESCE(device_id::text, ''), COALESCE(encode(device_public_key, 'hex'), '')
			FROM enrollment_tokens WHERE secret_sha256 = sha256($1::bytea)`, []byte(token)).Scan(&r.used, &r.device, &r.key)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	setStatus := func(status accountsv1.BillingStatus) {
		fake.SetAccountStatus(billingAccountA, status, env.Clock.Now())
		pollBilling(t, env)
	}

	// Allowed: consume one token for a device, and one with a recorded device
	// that is later cleared, to cover a consumed token without a device ID.
	pub := newKey()
	token := newToken()
	first, err := enroll(token, pub)
	if err != nil {
		t.Fatalf("first enrollment while allowed: %v", err)
	}
	noDeviceToken := newToken()
	noDevicePub := newKey()
	if _, err := enroll(noDeviceToken, noDevicePub); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Pool.Exec(ctx, `UPDATE enrollment_tokens SET device_id = NULL WHERE secret_sha256 = sha256($1::bytea)`, []byte(noDeviceToken)); err != nil {
		t.Fatal(err)
	}
	devices := deviceCount()
	consumed := tokenState(token)
	noDevice := tokenState(noDeviceToken)
	if !consumed.used || consumed.device != first.GetDeviceId() || noDevice.device != "" {
		t.Fatalf("token state: consumed=%+v noDevice=%+v", consumed, noDevice)
	}

	// retries checks every kind of retry of the consumed tokens in the current
	// billing phase. A nil want means that the same-key retry returns the
	// original device; otherwise it must fail with the billing reason.
	retries := func(phase string, wantBilling ownsandv1.BillingReason) {
		t.Helper()
		// Another key: invalid in every phase, with no billing detail.
		_, err := enroll(token, newKey())
		requireInvalidEnrollmentToken(t, phase+": consumed token with another key", err)
		// No recorded device: invalid even with the recorded key.
		_, err = enroll(noDeviceToken, noDevicePub)
		requireInvalidEnrollmentToken(t, phase+": consumed token without a device", err)
		// The same key is an eligible retry.
		resp, err := enroll(token, pub)
		if wantBilling == ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
			if err != nil || resp.GetDeviceId() != first.GetDeviceId() || resp.GetOrganizationId() != org.ID {
				t.Fatalf("%s: same-key retry = %v, %v; want device %s", phase, resp, err, first.GetDeviceId())
			}
		} else if got := billingReasonOf(t, phase+": same-key retry", err); got != wantBilling {
			t.Fatalf("%s: same-key retry: billing reason %v, want %v (err %v)", phase, got, wantBilling, err)
		}
		// None of the calls creates a device or changes a token.
		if got := deviceCount(); got != devices {
			t.Fatalf("%s: devices = %d, want %d", phase, got, devices)
		}
		if got := tokenState(token); got != consumed {
			t.Fatalf("%s: consumed token changed: %+v, want %+v", phase, got, consumed)
		}
		if got := tokenState(noDeviceToken); got != noDevice {
			t.Fatalf("%s: token without a device changed: %+v, want %+v", phase, got, noDevice)
		}
	}
	retries("allowed", ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED)

	pending := newToken()
	for _, tc := range []struct {
		phase  string
		status accountsv1.BillingStatus
		reason ownsandv1.BillingReason
	}{
		{"blocked", accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, ownsandv1.BillingReason_BILLING_REASON_BLOCKED},
		{"suspended", accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, ownsandv1.BillingReason_BILLING_REASON_SUSPENDED},
	} {
		setStatus(tc.status)
		retries(tc.phase, tc.reason)
		// A first enrollment is still gated by billing and leaves the token
		// and the devices as they were.
		_, err := enroll(pending, newKey())
		if got := billingReasonOf(t, tc.phase+": first enrollment", err); got != tc.reason {
			t.Fatalf("%s: first enrollment: billing reason %v, want %v (err %v)", tc.phase, got, tc.reason, err)
		}
		if got := deviceCount(); got != devices {
			t.Fatalf("%s: first enrollment created a device: %d, want %d", tc.phase, got, devices)
		}
		if got := tokenState(pending); got.used || got.device != "" || got.key != "" {
			t.Fatalf("%s: first enrollment consumed the token: %+v", tc.phase, got)
		}
	}

	// Recovery: the same results as in the first allowed phase, and the
	// pending token enrolls a new device.
	setStatus(accountsv1.BillingStatus_BILLING_STATUS_ALLOWED)
	retries("recovered", ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED)
	fresh, err := enroll(pending, newKey())
	if err != nil {
		t.Fatalf("first enrollment after the recovery: %v", err)
	}
	if fresh.GetDeviceId() == first.GetDeviceId() || deviceCount() != devices+1 || !tokenState(pending).used {
		t.Fatalf("first enrollment after the recovery: device %s, devices %d, token %+v", fresh.GetDeviceId(), deviceCount(), tokenState(pending))
	}
}
