// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// consumedNonceCount returns the number of replay records.
func consumedNonceCount(t *testing.T, env *testenv.Env) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM consumed_nonces").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNonceDeadlineIsEnforcedAtTheDeviceRowLock holds the device row lock so
// that a device RPC passes its first nonce check, waits for the lock, and then
// moves the clock while the RPC waits. The RPC has to check the deadline again
// after it got the lock: a nonce that expired meanwhile is rejected and not
// consumed, and a nonce that is still valid at the locked point is consumed
// exactly once.
func TestNonceDeadlineIsEnforcedAtTheDeviceRowLock(t *testing.T) {
	type rpc func(env *testenv.Env, d *testenv.Device, nonce []byte) error
	authenticate := func(env *testenv.Env, d *testenv.Device, nonce []byte) error {
		_, err := env.Clients("").Agent.Authenticate(context.Background(), connect.NewRequest(&ownsandv1.AuthenticateRequest{
			DeviceId: d.ID, Nonce: nonce, Signature: d.Sign(auth.PurposeAuthenticate, nonce), StateEpoch: d.Epoch,
		}))
		return err
	}
	confirmRemoval := func(env *testenv.Env, d *testenv.Device, nonce []byte) error {
		_, err := env.Clients("").Agent.ConfirmRemoval(context.Background(), connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
			DeviceId: d.ID, Nonce: nonce, Signature: d.Sign(auth.PurposeConfirmRemoval, nonce),
		}))
		return err
	}
	cases := []struct {
		name    string
		call    rpc
		advance time.Duration
		expired bool
		// removed removes the device first, so that ConfirmRemoval succeeds
		// for a nonce that is valid at the locked point.
		removed bool
	}{
		{name: "authenticate expired at the lock", call: authenticate, advance: auth.NonceLifetime, expired: true},
		{name: "authenticate long after the deadline", call: authenticate, advance: auth.NonceLifetime + time.Minute, expired: true},
		{name: "authenticate one millisecond before the deadline", call: authenticate, advance: auth.NonceLifetime - time.Millisecond},
		{name: "confirm removal expired at the lock", call: confirmRemoval, advance: auth.NonceLifetime, expired: true},
		{name: "confirm removal one millisecond before the deadline", call: confirmRemoval, advance: auth.NonceLifetime - time.Millisecond, removed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			org := env.NewOrg("alice")
			dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
			if tc.removed {
				if _, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID})); err != nil {
					t.Fatal(err)
				}
			}
			ch, err := env.Clients("").Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: dev.ID}))
			if err != nil {
				t.Fatal(err)
			}
			nonce := ch.Msg.GetNonce()
			before := consumedNonceCount(t, env)

			lock, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					_ = lock.Rollback(ctx)
				}
			}
			defer release()
			var locked string
			if err := lock.QueryRow(ctx, "SELECT device_id::text FROM devices WHERE device_id = $1 FOR UPDATE", dev.ID).Scan(&locked); err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)
			go func() { result <- tc.call(env, dev, nonce) }()
			waitForLockWaiter(t, env, "FROM devices", "FOR UPDATE")
			// The RPC passed its first nonce check and waits for the row.
			env.Clock.Advance(tc.advance)
			release()

			err = <-result
			if tc.expired {
				if testenv.Code(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "invalid or expired nonce") {
					t.Fatalf("nonce that expired while the RPC waited for the lock: %v", err)
				}
				if got := consumedNonceCount(t, env); got != before {
					t.Fatalf("consumed nonces = %d, want %d: an expired nonce was consumed", got, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("nonce that is valid at the locked point: %v", err)
			}
			if got := consumedNonceCount(t, env); got != before+1 {
				t.Fatalf("consumed nonces = %d, want %d", got, before+1)
			}
		})
	}
}

// TestFreshNonceIsValidExactlyOnce checks that the locked deadline check keeps
// a fresh, correctly signed nonce valid for one use and that a replay is still
// reported as a used nonce.
func TestFreshNonceIsValidExactlyOnce(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	agent := env.Clients("").Agent
	ch, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: dev.ID}))
	if err != nil {
		t.Fatal(err)
	}
	nonce := ch.Msg.GetNonce()
	req := &ownsandv1.AuthenticateRequest{DeviceId: dev.ID, Nonce: nonce, Signature: dev.Sign(auth.PurposeAuthenticate, nonce), StateEpoch: dev.Epoch}
	if _, err := agent.Authenticate(ctx, connect.NewRequest(req)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	_, err = agent.Authenticate(ctx, connect.NewRequest(req))
	if testenv.Code(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "nonce already used") {
		t.Fatalf("replay: %v", err)
	}
}
