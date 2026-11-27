// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

type presenceClient struct {
	accountsv1connect.UsageServiceClient
	lookup func(context.Context, *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error)
}

func (p presenceClient) GetOrganizationBilling(ctx context.Context, req *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error) {
	return p.lookup(ctx, req)
}

func TestLinkPresenceRotatesPastErrorsAndStopsOnLeaseLoss(t *testing.T) {
	ctx := t.Context()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, start)
	orgs := make([]string, 34)
	for i := range orgs {
		orgs[i] = fmt.Sprintf("0190f3a4-0000-7000-8000-%012x", i)
		f.addOrg(t, orgs[i])
		account := accountA
		if i == len(orgs)-1 {
			account = accountB
		}
		f.link(t, orgs[i], account)
	}
	f.clock.Advance(time.Hour)
	f.fake.SetAccountStatus(accountB, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.clock.Now())
	if err := f.statuses.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	lastID := orgs[len(orgs)-1]
	blockStart := *f.load(t, lastID).BlockedSince
	for _, id := range []string{orgs[0], orgs[1], lastID} {
		if _, err := f.fake.Client().UnlinkOrganization(ctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{Service: billing.ServiceID, OrganizationId: id})); err != nil {
			t.Fatal(err)
		}
	}

	f.clock.Advance(2 * time.Hour)
	var checked []string
	held := true
	client := presenceClient{UsageServiceClient: f.fake.Client(), lookup: func(ctx context.Context, req *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error) {
		id := req.Msg.GetOrganizationId()
		checked = append(checked, id)
		switch id {
		case orgs[0]:
			held = false
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("injected transient failure"))
		case orgs[1]:
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("injected authorization failure"))
		}
		return f.fake.Client().GetOrganizationBilling(ctx, req)
	}}
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: client, Ownership: func() (uint64, bool) { return 0, held }})
	changes := 0
	statuses.OnChange(func() { changes++ })

	check := func() {
		t.Helper()
		if err := statuses.CheckLinkPresence(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if len(checked) != 1 || checked[0] != orgs[0] {
		t.Fatalf("checks after lease loss = %v", checked)
	}
	check()
	if len(checked) != 1 {
		t.Fatalf("scan ran without lease: %v", checked)
	}
	held = true
	check()
	if len(checked) != 33 || checked[32] != orgs[32] || !f.load(t, lastID).Linked() {
		t.Fatalf("first full page should leave the final row for the next pass: %v", checked)
	}
	check()
	st := f.load(t, lastID)
	if len(checked) != len(orgs) || st.Linked() || !st.BlockedSince.Equal(blockStart) || changes != 1 {
		t.Fatalf("last status = %+v, checks = %d, hooks = %d", st, len(checked), changes)
	}
	for _, id := range orgs[:len(orgs)-1] {
		if !f.load(t, id).Linked() {
			t.Errorf("presence scan removed %s without confirmed absence", id)
		}
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 3 {
		t.Fatalf("presence scan performed an upstream unlink, total calls = %d", n)
	}
	check()
	if len(checked) != len(orgs)+1 || checked[len(orgs)] != orgs[0] {
		t.Fatalf("scan did not wrap to the first row, got %v", checked)
	}
}

func TestOwnerUnlinkIntentCannotRemoveSameAccountReplacement(t *testing.T) {
	ctx := t.Context()
	f := newStatusFixture(t, time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	writer := billing.NewStatuses(billing.StatusOptions{DB: f.db, Client: f.fake.Client(), Logger: f.logger, Now: f.clock.Now, WriteOwnerUnlinks: true})
	const oldID = "0190f3a4-0000-7000-8000-000000000123"
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", org1); err != nil {
			return err
		}
		_, err := writer.PrepareOwnerUnlinkTx(ctx, tx, org1, accountA, oldID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.statuses.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.link(t, org1, accountA)
	err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", org1); err != nil {
			return err
		}
		_, _, err := writer.ResolveOwnerUnlinkTx(ctx, tx, org1, oldID)
		return err
	})
	if !errors.Is(err, billing.ErrOwnerUnlinkSuperseded) {
		t.Fatalf("old owner unlink = %v", err)
	}
	if _, _, ok := f.fake.Link(org1); !ok || !f.load(t, org1).Linked() || f.fake.Calls(accountstest.MethodUnlinkOrganization) != 1 {
		t.Fatal("the old intent affected the replacement")
	}
}
