// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestTombstoneDeadlineIsEnforcedAtTheDeviceRowLock(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	agent := env.Clients("").Agent
	for _, rpc := range []struct {
		name    string
		call    func(context.Context, *testenv.Device, []byte) error
		deletes bool
	}{
		{
			name: "authenticate",
			call: func(ctx context.Context, dev *testenv.Device, nonce []byte) error {
				resp, err := agent.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
					DeviceId: dev.ID, Nonce: nonce, Signature: dev.Sign(auth.PurposeAuthenticate, nonce), StateEpoch: dev.Epoch,
				}))
				if err == nil && (resp.Msg.GetRemovalNotice() == nil || resp.Msg.GetSession() != nil) {
					return fmt.Errorf("expected only a removal notice")
				}
				return err
			},
		},
		{
			name: "confirm removal",
			call: func(ctx context.Context, dev *testenv.Device, nonce []byte) error {
				_, err := agent.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
					DeviceId: dev.ID, Nonce: nonce, Signature: dev.Sign(auth.PurposeConfirmRemoval, nonce),
				}))
				return err
			},
			deletes: true,
		},
	} {
		for _, boundary := range []struct {
			name   string
			offset time.Duration
		}{
			{name: "before expiry", offset: -time.Millisecond},
			{name: "at expiry"},
			{name: "after expiry", offset: time.Millisecond},
		} {
			t.Run(rpc.name+"/"+boundary.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
				if err := removeDevice(org, dev.ID); err != nil {
					t.Fatal(err)
				}
				// Reach the tombstone deadline during the lock wait while the nonce is still valid.
				const untilExpiry = 10 * time.Second
				if _, err := env.DB.Pool.Exec(ctx, "UPDATE devices SET tombstone_expire_time = $2 WHERE device_id = $1",
					dev.ID, env.Clock.Now().Add(untilExpiry)); err != nil {
					t.Fatal(err)
				}
				ch, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: dev.ID}))
				if err != nil {
					t.Fatal(err)
				}
				nonce := ch.Msg.GetNonce()
				wantNonces := consumedNonceCount(t, env)

				lock, err := env.DB.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lock.Rollback(context.Background()) }()
				if _, err := lock.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", dev.ID); err != nil {
					t.Fatal(err)
				}

				result := make(chan error, 1)
				go func() { result <- rpc.call(ctx, dev, nonce) }()
				waitForLockWaiter(t, env, "FROM devices", "FOR UPDATE")
				env.Clock.Advance(untilExpiry + boundary.offset)
				if err := lock.Rollback(ctx); err != nil {
					t.Fatal(err)
				}

				err = <-result
				wantRow := true
				if boundary.offset < 0 {
					if err != nil {
						t.Fatalf("visible tombstone: %v", err)
					}
					wantNonces++
					wantRow = !rpc.deletes
				} else if testenv.Code(err) != connect.CodeNotFound {
					t.Errorf("expired tombstone: got %v, want not_found", err)
				}
				if got := consumedNonceCount(t, env); got != wantNonces {
					t.Errorf("consumed nonces = %d, want %d", got, wantNonces)
				}
				var rowExists bool
				if err := env.DB.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM devices WHERE device_id = $1)", dev.ID).Scan(&rowExists); err != nil {
					t.Fatal(err)
				}
				if rowExists != wantRow {
					t.Errorf("tombstone exists = %t, want %t", rowExists, wantRow)
				}
			})
		}
	}
}
