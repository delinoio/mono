// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func TestGetOrganizationBillingRefreshesExpiredCredit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasTopUp bool
	}{
		{name: "last grant expires"},
		{name: "another grant remains", hasTopUp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			f := newFixture(t, env)
			subscribe(t, env, f)
			org := newOrg(t)
			link(t, env, f.AccountID, org, f.Owner.ID, "Expiring credit")

			var expires time.Time
			if err := env.DB.Pool.QueryRow(ctx(), `SELECT expire_time FROM credit_grants
				WHERE billing_account_id = $1 AND source = 'subscription_credit'`, f.AccountID).Scan(&expires); err != nil {
				t.Fatal(err)
			}
			beforeBalance := 10 * usd
			if tc.hasTopUp {
				env.Clock.Advance(time.Hour)
				if _, err := env.Credit(f.Owner.ID).CreateTopUp(ctx(), connect.NewRequest(&accountsv1.CreateTopUpRequest{
					BillingAccountId: f.AccountID, IdempotencyKey: newKey(t), AmountUsdMicros: 25 * usd,
				})); err != nil {
					t.Fatal(err)
				}
				charge := env.Paddle.Charges()[0]
				if code := env.Webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
					ID: env.Paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge,
					TopUpID: paddletest.ChargeTopUpID(charge), SubtotalCents: 2500, TaxCents: 250,
				})); code != http.StatusOK {
					t.Fatalf("top-up webhook status = %d", code)
				}
				beforeBalance += 25 * usd
			}

			read := func() *accountsv1.GetOrganizationBillingResponse {
				t.Helper()
				resp, err := env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
					Service: ownsand, OrganizationId: org,
				}))
				if err != nil {
					t.Fatal(err)
				}
				return resp.Msg
			}
			setClock := func(at time.Time) {
				env.Clock.Advance(at.Sub(env.Clock.Now()) - time.Millisecond)
			}
			setClock(expires.Add(-time.Microsecond))
			before := read()
			if before.GetBalanceUsdMicros() != beforeBalance || before.GetStatus().GetStatus() != allowed {
				t.Fatalf("before expiry = %v", before)
			}

			setClock(expires)
			atExpiry := read()
			status := atExpiry.GetStatus()
			if tc.hasTopUp {
				if atExpiry.GetBalanceUsdMicros() != 25*usd || !proto.Equal(status, before.GetStatus()) {
					t.Fatalf("at expiry with remaining credit = %v", atExpiry)
				}
				return
			}
			suspends := expires.Add(billing.SuspendAfter)
			if atExpiry.GetBalanceUsdMicros() != 0 || status.GetStatus() != blocked ||
				status.GetReason() != accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NON_POSITIVE_BALANCE ||
				!status.GetBlockedSince().AsTime().Equal(expires) || !status.GetSuspendTime().AsTime().Equal(suspends) ||
				status.GetSequence() <= before.GetStatus().GetSequence() {
				t.Fatalf("at expiry = %v", atExpiry)
			}
			changes := poll(t, env, before.GetStatus().GetSequence(), 10).GetStatuses()
			if len(changes) != 1 || !proto.Equal(changes[0], status) {
				t.Fatalf("poll after expiry = %v", changes)
			}
			if again := read(); !proto.Equal(again.GetStatus(), status) {
				t.Fatalf("repeated read = %v", again)
			}
			if kinds := ledgerKinds(t, env, f.Owner.ID, f.AccountID); len(kinds) != 1 || kinds[0] != "SUBSCRIPTION_CREDIT" {
				t.Fatalf("ledger before expiry job = %v", kinds)
			}
			if err := env.Billing.ExpireGrants(ctx()); err != nil {
				t.Fatal(err)
			}
			if afterJob := read(); afterJob.GetBalanceUsdMicros() != 0 || !proto.Equal(afterJob.GetStatus(), status) {
				t.Fatalf("after expiry job = %v", afterJob)
			}
			if kinds := ledgerKinds(t, env, f.Owner.ID, f.AccountID); len(kinds) != 2 || kinds[0] != "EXPIRY" || kinds[1] != "SUBSCRIPTION_CREDIT" {
				t.Fatalf("ledger after expiry job = %v", kinds)
			}
			var notifications int
			if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications
				WHERE billing_account_id = $1 AND kind = $2`, f.AccountID, string(billing.NotifyOrganizationBlocked)).Scan(&notifications); err != nil {
				t.Fatal(err)
			}
			if notifications != 1 {
				t.Fatalf("blocked notifications = %d, want 1", notifications)
			}

			setClock(suspends)
			suspended := read().GetStatus()
			if suspended.GetStatus() != accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED ||
				suspended.GetSequence() <= status.GetSequence() ||
				!proto.Equal(suspended.GetBlockedSince(), status.GetBlockedSince()) ||
				!proto.Equal(suspended.GetSuspendTime(), status.GetSuspendTime()) {
				t.Fatalf("at suspension = %v", suspended)
			}
		})
	}
}

func TestGetOrganizationBillingRechecksLinkAfterLockWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		relink bool
	}{
		{name: "unlink"},
		{name: "relink", relink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			f := newFixture(t, env)
			subscribe(t, env, f)
			org := newOrg(t)
			link(t, env, f.AccountID, org, f.Owner.ID, "Original")
			replacement := createAccount(t, f.Owner, "Replacement").GetBillingAccountId()

			var response *connect.Response[accountsv1.GetOrganizationBillingResponse]
			err := raceMembershipChange(t, env, f.AccountID, func(tx pgx.Tx) {
				now := env.Clock.Now()
				execTx(t, tx, `UPDATE organization_links SET unlink_time = $2
					WHERE service = 'ownsand' AND organization_id = $1 AND unlink_time IS NULL`, org, now)
				if tc.relink {
					if err := billing.LockAccount(ctx(), tx, replacement); err != nil {
						t.Fatal(err)
					}
					execTx(t, tx, `INSERT INTO organization_links
						(organization_link_id, service, organization_id, billing_account_id, organization_name, link_time)
						VALUES ($1, 'ownsand', $2, $3, 'Replacement', $4)`, newKey(t), org, replacement, now)
					if err := env.Billing.SyncStatuses(ctx(), tx, replacement); err != nil {
						t.Fatal(err)
					}
				}
			}, func() error {
				var err error
				response, err = env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
					Service: ownsand, OrganizationId: org,
				}))
				return err
			})
			if !tc.relink {
				wantCode(t, err, connect.CodeNotFound)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := response.Msg; got.GetBillingAccountId() != replacement || got.GetStatus().GetBillingAccountId() != replacement ||
				got.GetBillingAccountName() != "Replacement" || got.GetBalanceUsdMicros() != 0 || got.GetStatus().GetStatus() != blocked {
				t.Fatalf("billing after relink = %v", got)
			}
		})
	}
}
