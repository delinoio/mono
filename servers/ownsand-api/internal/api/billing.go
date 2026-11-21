// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/validate"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// maxAccountsPageToken bounds the page token that ListLinkableBillingAccounts
// passes to delino-accounts.
const maxAccountsPageToken = 1024

// billingRestriction names the enforcement list of a gated RPC
// (docs/servers-ownsand-api-foundation.md#enforcement).
type billingRestriction int

const (
	// restrictWhenBlocked: rejected while the organization is BLOCKED or
	// SUSPENDED (CreateSandbox, ResumeSandbox, CreateSnapshot,
	// ExportSnapshot, CreateTemplate, CreateEnrollmentToken and Enroll).
	restrictWhenBlocked billingRestriction = iota + 1
	// restrictWhenSuspended: rejected only while the organization is
	// SUSPENDED (Exec, AttachExec, ReadFile, WriteFile, OpenSession and
	// AuthorizeRelay).
	restrictWhenSuspended
)

// checkBilling applies billing enforcement to a gated RPC of the
// organization. It reads only the status cache and runs after
// authentication and authorization and before any record changes. A
// rejected call fails with resource_exhausted and an ErrorDetail with the
// billing reason.
func (s *Service) checkBilling(ctx context.Context, q db.Querier, orgID string, r billingRestriction) error {
	if s.Billing == nil {
		return nil
	}
	st, phase, err := s.Billing.Enforced(ctx, q, orgID)
	if err != nil {
		return s.internal(ctx, "load billing status", err)
	}
	if phase == billing.PhaseSuspended || (phase == billing.PhaseBlocked && r == restrictWhenBlocked) {
		reason := billing.ReasonOf(phase, st.Linked())
		s.Logger.InfoContext(ctx, "billing_rpc_rejected", slog.String("organization_id", orgID), slog.String("phase", billing.PhaseName(phase)),
			slog.String("reason", reason.String()))
		return errBilling(reason)
	}
	return nil
}

// billingMessage returns the message of a billing error.
func billingMessage(reason ownsandv1.BillingReason) string {
	switch reason {
	case ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED:
		return "billing account required: the organization has no linked billing account; the organization owner links one in the organization settings, and billing is managed at " + billing.AccountsWebURL
	case ownsandv1.BillingReason_BILLING_REASON_BLOCKED:
		return "billing blocked: the linked billing account has no active subscription or a balance of 0 or less; manage billing at " + billing.AccountsWebURL
	case ownsandv1.BillingReason_BILLING_REASON_SUSPENDED:
		return "billing suspended: non-payment continued for 3 days, so running sandboxes are paused and the data path is blocked; manage billing at " + billing.AccountsWebURL
	}
	return "billing restriction; manage billing at " + billing.AccountsWebURL
}

// errBilling returns the resource_exhausted error of a call that billing
// enforcement rejected. Its ErrorDetail carries the billing reason and no
// retry delay: callers must not retry it.
func errBilling(reason ownsandv1.BillingReason) error {
	msg := billingMessage(reason)
	e := connect.NewError(connect.CodeResourceExhausted, errors.New(msg))
	if d, err := connect.NewErrorDetail(&ownsandv1.ErrorDetail{
		Code:          ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
		Message:       msg,
		BillingReason: reason,
	}); err == nil {
		e.AddDetail(d)
	}
	return e
}

// accountsError maps a failed delino-accounts call. The codes in pass reach
// the caller unchanged; any other failure means that delino-accounts cannot
// serve the call now, which is unavailable.
func (s *Service) accountsError(ctx context.Context, op string, err error, pass ...connect.Code) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	code := connect.CodeOf(err)
	for _, c := range pass {
		if code == c {
			msg := err.Error()
			if ce := new(connect.Error); errors.As(err, &ce) {
				msg = ce.Message()
			}
			return connect.NewError(code, errors.New(msg))
		}
	}
	s.Logger.WarnContext(ctx, "billing_accounts_call_failed", slog.String("op", op), slog.String("code", code.String()), slog.String("error", err.Error()))
	return errUnavailable("delino-accounts is unavailable; try again later")
}

// usageClient returns the delino-accounts client or unavailable without
// one.
func (s *Service) usageClient() (accountsClient, error) {
	if s.Billing == nil || s.Billing.Client() == nil {
		return nil, errUnavailable("delino-accounts is not configured")
	}
	return s.Billing.Client(), nil
}

// accountsClient is the part of the UsageService client that the billing
// RPCs use.
type accountsClient interface {
	LinkOrganization(context.Context, *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error)
	UnlinkOrganization(context.Context, *connect.Request[accountsv1.UnlinkOrganizationRequest]) (*connect.Response[accountsv1.UnlinkOrganizationResponse], error)
	ListManageableBillingAccounts(context.Context, *connect.Request[accountsv1.ListManageableBillingAccountsRequest]) (*connect.Response[accountsv1.ListManageableBillingAccountsResponse], error)
	GetOrganizationBilling(context.Context, *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error)
}

// BillingHandler serves BillingService. Its RPCs are never billed and never
// rejected by billing enforcement.
type BillingHandler struct{ *Service }

var _ ownsandv1connect.BillingServiceHandler = BillingHandler{}

func (h BillingHandler) statuses() (*billing.Statuses, error) {
	if h.Billing == nil {
		return nil, errUnavailable("billing is not configured")
	}
	return h.Billing, nil
}

func (h BillingHandler) GetBillingStatus(ctx context.Context, req *connect.Request[ownsandv1.GetBillingStatusRequest]) (*connect.Response[ownsandv1.GetBillingStatusResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadBillingStatus); err != nil {
		return nil, err
	}
	statuses, err := h.statuses()
	if err != nil {
		return nil, err
	}
	st, _, err := statuses.Enforced(ctx, h.DB.Pool, orgID)
	if err != nil {
		return nil, h.internal(ctx, "get billing status", err)
	}
	return connect.NewResponse(&ownsandv1.GetBillingStatusResponse{Billing: st.Proto(h.now())}), nil
}

func (h BillingHandler) GetBillingUsage(ctx context.Context, req *connect.Request[ownsandv1.GetBillingUsageRequest]) (*connect.Response[ownsandv1.GetBillingUsageResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadBillingUsage); err != nil {
		return nil, err
	}
	statuses, err := h.statuses()
	if err != nil {
		return nil, err
	}
	st, err := statuses.Load(ctx, h.DB.Pool, orgID)
	if err != nil {
		return nil, h.internal(ctx, "get billing usage", err)
	}
	if !st.Linked() {
		return nil, errPrecondition("the organization has no linked billing account")
	}
	client, err := h.usageClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.GetOrganizationBilling(ctx, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{Service: billing.ServiceID, OrganizationId: orgID}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, errPrecondition("the organization has no linked billing account")
	}
	if err != nil {
		return nil, h.accountsError(ctx, "GetOrganizationBilling", err)
	}
	m := resp.Msg
	monthStart := m.GetMonthStart()
	if monthStart == nil {
		now := h.now()
		monthStart = timestamppb.New(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	}
	out := &ownsandv1.GetBillingUsageResponse{
		BillingAccountId: m.GetBillingAccountId(), BillingAccountName: m.GetBillingAccountName(),
		BalanceUsdMicros: m.GetBalanceUsdMicros(), MonthStart: monthStart,
	}
	byMeter := map[accountsv1.Meter]*accountsv1.MeterUsage{}
	for _, u := range m.GetMeterUsages() {
		byMeter[u.GetMeter()] = u
	}
	for _, meter := range billing.Meters {
		u := byMeter[billing.AccountsMeter(meter)]
		out.MeterUsages = append(out.MeterUsages, &ownsandv1.BillingMeterUsage{Meter: meter, Quantity: u.GetQuantity(), AmountUsdMicros: u.GetAmountUsdMicros()})
	}
	return connect.NewResponse(out), nil
}

// billingRole maps a delino-accounts billing account role.
func billingRole(r accountsv1.BillingAccountRole) ownsandv1.BillingAccountRole {
	switch r {
	case accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER:
		return ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER
	case accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN:
		return ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN
	case accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER:
		return ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER
	}
	return ownsandv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_UNSPECIFIED
}

func (h BillingHandler) ListLinkableBillingAccounts(ctx context.Context, req *connect.Request[ownsandv1.ListLinkableBillingAccountsRequest]) (*connect.Response[ownsandv1.ListLinkableBillingAccountsResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	p, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionManageBilling)
	if err != nil {
		return nil, err
	}
	size, err := validate.PageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, errInvalid(err)
	}
	if len(req.Msg.GetPageToken()) > maxAccountsPageToken {
		return nil, errInvalidf("invalid page_token")
	}
	client, err := h.usageClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.ListManageableBillingAccounts(ctx, connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
		UserId: p.ID, PageSize: size, PageToken: req.Msg.GetPageToken(),
	}))
	if err != nil {
		return nil, h.accountsError(ctx, "ListManageableBillingAccounts", err, connect.CodeInvalidArgument)
	}
	out := &ownsandv1.ListLinkableBillingAccountsResponse{NextPageToken: resp.Msg.GetNextPageToken()}
	for _, a := range resp.Msg.GetBillingAccounts() {
		out.BillingAccounts = append(out.BillingAccounts, &ownsandv1.LinkableBillingAccount{
			BillingAccountId: a.GetBillingAccountId(), Name: a.GetName(), Role: billingRole(a.GetCallerRole()),
		})
	}
	return connect.NewResponse(out), nil
}

// authorizeBillingOwnerLocked authorizes LinkBillingAccount and
// UnlinkBillingAccount: an unlocked pre-check, then the organization row
// lock FOR NO KEY UPDATE and the authoritative owner check. The lock
// serializes the link RPCs with each other, with TransferOwnership and
// DeleteOrganization (FOR UPDATE) and with the pending unlinks, while the
// creates of the organization (FOR KEY SHARE) keep running.
func (s *Service) authorizeBillingOwnerLocked(ctx context.Context, tx pgx.Tx, orgID string) (auth.Principal, error) {
	if _, _, err := s.authorize(ctx, tx, orgID, authz.ActionManageBilling); err != nil {
		return auth.Principal{}, err
	}
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, errNotFound("organization")
	}
	if err != nil {
		return auth.Principal{}, err
	}
	p, _, err := s.authorize(ctx, tx, orgID, authz.ActionManageBilling)
	return p, err
}

func (h BillingHandler) LinkBillingAccount(ctx context.Context, req *connect.Request[ownsandv1.LinkBillingAccountRequest]) (*connect.Response[ownsandv1.LinkBillingAccountResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	var st billing.Status
	var change billing.Change
	var accountName string
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, err := h.authorizeBillingOwnerLocked(ctx, tx, orgID)
		if err != nil {
			return err
		}
		accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
		if err != nil {
			return err
		}
		client, err := h.usageClient()
		if err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(ctx, "SELECT name FROM organizations WHERE organization_id = $1", orgID).Scan(&name); err != nil {
			return err
		}
		cur, ok, err := billing.LoadStatus(ctx, tx, orgID, true)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound("organization")
		}
		if cur.Linked() && cur.BillingAccountID != accountID {
			return errAlreadyExists("the organization is linked to another billing account; unlink it first")
		}
		// An unlink that an ownership transfer or a deletion left pending
		// must reach delino-accounts first, or its retry would remove this
		// link.
		now := h.now()
		if err := h.Billing.UnlinkPendingTx(ctx, tx, orgID, now); err != nil {
			if errors.Is(err, billing.ErrUnlinkFailed) {
				return errUnavailable("delino-accounts is unavailable; try again later")
			}
			return err
		}
		resp, err := client.LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{
			Service: billing.ServiceID, OrganizationId: orgID, BillingAccountId: accountID, UserId: p.ID, OrganizationName: name,
		}))
		if err != nil {
			return h.accountsError(ctx, "LinkOrganization", err, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeAlreadyExists, connect.CodeInvalidArgument)
		}
		accountName = resp.Msg.GetBillingAccountName()
		st, change, err = billing.LinkTx(ctx, tx, orgID, accountID, resp.Msg.GetStatus(), h.now())
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "link billing account", err)
	}
	h.Billing.Committed(ctx, change)
	return connect.NewResponse(&ownsandv1.LinkBillingAccountResponse{Billing: st.Proto(h.now()), BillingAccountName: accountName}), nil
}

func (h BillingHandler) UnlinkBillingAccount(ctx context.Context, req *connect.Request[ownsandv1.UnlinkBillingAccountRequest]) (*connect.Response[ownsandv1.UnlinkBillingAccountResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	var st billing.Status
	var change billing.Change
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := h.authorizeBillingOwnerLocked(ctx, tx, orgID); err != nil {
			return err
		}
		if _, err := h.statuses(); err != nil {
			return err
		}
		cur, ok, err := billing.LoadStatus(ctx, tx, orgID, true)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound("organization")
		}
		if !cur.Linked() {
			// Idempotent: nothing to remove.
			st = cur
			return nil
		}
		client, err := h.usageClient()
		if err != nil {
			return err
		}
		if _, err := client.UnlinkOrganization(ctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{Service: billing.ServiceID, OrganizationId: orgID})); err != nil {
			return h.accountsError(ctx, "UnlinkOrganization", err)
		}
		// delino-accounts has no link now, so an older pending unlink has
		// nothing left to remove.
		if _, err := tx.Exec(ctx, "DELETE FROM billing_pending_unlinks WHERE organization_id = $1", orgID); err != nil {
			return err
		}
		st, _, change, err = billing.UnlinkTx(ctx, tx, orgID, billing.SourceUnlink, h.now())
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "unlink billing account", err)
	}
	h.Billing.Committed(ctx, change)
	return connect.NewResponse(&ownsandv1.UnlinkBillingAccountResponse{Billing: st.Proto(h.now())}), nil
}

// removeBillingLinkTx removes the billing link of the organization in the
// transaction of TransferOwnership or DeleteOrganization and records a
// pending unlink for delino-accounts. It returns the change and whether an
// unlink is pending; after the commit, finishBillingUnlink sends it.
func (s *Service) removeBillingLinkTx(ctx context.Context, tx pgx.Tx, orgID, source string) (billing.Change, bool, error) {
	if s.Billing == nil {
		return billing.Change{}, false, nil
	}
	now := s.now()
	_, former, change, err := billing.UnlinkTx(ctx, tx, orgID, source, now)
	if err != nil || former == "" {
		return billing.Change{}, false, err
	}
	if err := billing.AddPendingUnlinkTx(ctx, tx, orgID, former, now); err != nil {
		return billing.Change{}, false, err
	}
	return change, true, nil
}

// finishBillingUnlink logs a committed link removal and sends its pending
// unlink once; when delino-accounts is unavailable, the lease holder
// retries it.
func (s *Service) finishBillingUnlink(ctx context.Context, orgID string, change billing.Change, pending bool) {
	if s.Billing == nil || !pending {
		return
	}
	s.Billing.Committed(ctx, change)
	if s.Billing.Client() == nil {
		s.Logger.WarnContext(ctx, "billing_unlink_pending", slog.String("organization_id", orgID), slog.String("detail", "delino-accounts is not configured"))
		return
	}
	if err := s.Billing.UnlinkPending(context.WithoutCancel(ctx), orgID); err != nil {
		s.Logger.WarnContext(ctx, "billing_unlink_pending", slog.String("organization_id", orgID), slog.String("error", fmt.Sprint(err)))
	}
}
