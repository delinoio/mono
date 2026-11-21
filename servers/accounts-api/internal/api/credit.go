// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
)

// CreditHandler implements CreditService. The billing package owns the
// credit rules; the handler checks the request fields, the membership and
// the role, in that order, and records billing audit events.
type CreditHandler struct {
	*Service
}

var _ accountsv1connect.CreditServiceHandler = (*CreditHandler)(nil)

// read checks a read RPC: the billing account ID and the membership.
func (h *CreditHandler) read(ctx context.Context, rawID string, p Permission) (string, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return "", err
	}
	accountID, err := normalizeID("billing_account_id", rawID)
	if err != nil {
		return "", err
	}
	account, err := loadAccess(ctx, h.db.Pool, accountID, userID, lockNone)
	if err != nil {
		return "", err
	}
	return accountID, require(account, p)
}

// change runs fn in a transaction after the membership and owner-or-admin
// role checks, which lock the billing account row. Callers check the request
// fields before, as the contract orders the checks.
func (h *CreditHandler) change(ctx context.Context, op, rawID string, fn func(tx pgx.Tx, accountID, userID string, now time.Time) error) error {
	userID, err := callerUserID(ctx)
	if err != nil {
		return err
	}
	accountID, err := normalizeID("billing_account_id", rawID)
	if err != nil {
		return err
	}
	now := h.timestamp()
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		return fn(tx, accountID, userID, now)
	})
	if err != nil {
		return h.internal(ctx, op, err)
	}
	return nil
}

// GetBalance reads the balance and the usable grants. Every member may call
// it.
func (h *CreditHandler) GetBalance(ctx context.Context, req *connect.Request[accountsv1.GetBalanceRequest]) (*connect.Response[accountsv1.GetBalanceResponse], error) {
	accountID, err := h.read(ctx, req.Msg.GetBillingAccountId(), PermissionRead)
	if err != nil {
		return nil, h.internal(ctx, "get_balance", err)
	}
	balance, grants, err := billing.Balance(ctx, h.db.Pool, accountID, h.timestamp())
	if err != nil {
		return nil, h.internal(ctx, "get_balance", err)
	}
	resp := &accountsv1.GetBalanceResponse{BalanceUsdMicros: balance}
	for _, g := range grants {
		resp.CreditGrants = append(resp.CreditGrants, g.Proto())
	}
	return connect.NewResponse(resp), nil
}

// ListLedgerEntries lists ledger entries, newest first. Every member may
// call it.
func (h *CreditHandler) ListLedgerEntries(ctx context.Context, req *connect.Request[accountsv1.ListLedgerEntriesRequest]) (*connect.Response[accountsv1.ListLedgerEntriesResponse], error) {
	const rpc = "ListLedgerEntries"
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, accountID)
	if err != nil {
		return nil, err
	}
	if tok.After != "" {
		if tok.After, err = normalizeID("page_token", tok.After); err != nil {
			return nil, invalidArgument("page_token is not valid for this request")
		}
	}
	if _, err := h.read(ctx, accountID, PermissionRead); err != nil {
		return nil, h.internal(ctx, "list_ledger_entries", err)
	}
	entries, err := billing.ListLedger(ctx, h.db.Pool, accountID, tok.After, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_ledger_entries", err)
	}
	resp := &accountsv1.ListLedgerEntriesResponse{}
	if len(entries) > size {
		entries = entries[:size]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: accountID, After: entries[size-1].ID})
	}
	for _, e := range entries {
		resp.LedgerEntries = append(resp.LedgerEntries, e.Proto())
	}
	return connect.NewResponse(resp), nil
}

// GetSubscription reads the subscription. Every member may call it.
func (h *CreditHandler) GetSubscription(ctx context.Context, req *connect.Request[accountsv1.GetSubscriptionRequest]) (*connect.Response[accountsv1.GetSubscriptionResponse], error) {
	accountID, err := h.read(ctx, req.Msg.GetBillingAccountId(), PermissionRead)
	if err != nil {
		return nil, h.internal(ctx, "get_subscription", err)
	}
	sub, err := billing.LoadSubscription(ctx, h.db.Pool, accountID, false)
	if err != nil {
		return nil, h.internal(ctx, "get_subscription", err)
	}
	return connect.NewResponse(&accountsv1.GetSubscriptionResponse{Subscription: sub.Proto()}), nil
}

// StartSubscription returns the Paddle checkout transaction of the
// subscription. Owner and admin only.
func (h *CreditHandler) StartSubscription(ctx context.Context, req *connect.Request[accountsv1.StartSubscriptionRequest]) (*connect.Response[accountsv1.StartSubscriptionResponse], error) {
	resp := &accountsv1.StartSubscriptionResponse{}
	err := h.change(ctx, "start_subscription", req.Msg.GetBillingAccountId(), func(tx pgx.Tx, accountID, userID string, now time.Time) error {
		txnID, sub, created, err := h.billing.StartSubscription(ctx, tx, accountID)
		if err != nil {
			return err
		}
		resp.PaddleTransactionId = txnID
		resp.Subscription = sub.Proto()
		if !created {
			return nil
		}
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditSubscriptionStarted, ActorUserID: userID,
			Details: map[string]string{"paddle_transaction_id": txnID},
		})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// CancelSubscription cancels at the end of the paid period. Owner and admin
// only.
func (h *CreditHandler) CancelSubscription(ctx context.Context, req *connect.Request[accountsv1.CancelSubscriptionRequest]) (*connect.Response[accountsv1.CancelSubscriptionResponse], error) {
	resp := &accountsv1.CancelSubscriptionResponse{}
	err := h.change(ctx, "cancel_subscription", req.Msg.GetBillingAccountId(), func(tx pgx.Tx, accountID, userID string, now time.Time) error {
		sub, changed, err := h.billing.CancelSubscription(ctx, tx, accountID)
		if err != nil {
			return err
		}
		resp.Subscription = sub.Proto()
		if !changed {
			return nil
		}
		return h.recordAudit(ctx, tx, now, auditEvent{BillingAccountID: accountID, Kind: AuditSubscriptionCanceled, ActorUserID: userID})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ResumeSubscription removes a scheduled cancellation. Owner and admin only.
func (h *CreditHandler) ResumeSubscription(ctx context.Context, req *connect.Request[accountsv1.ResumeSubscriptionRequest]) (*connect.Response[accountsv1.ResumeSubscriptionResponse], error) {
	resp := &accountsv1.ResumeSubscriptionResponse{}
	err := h.change(ctx, "resume_subscription", req.Msg.GetBillingAccountId(), func(tx pgx.Tx, accountID, userID string, now time.Time) error {
		sub, changed, err := h.billing.ResumeSubscription(ctx, tx, accountID)
		if err != nil {
			return err
		}
		resp.Subscription = sub.Proto()
		if !changed {
			return nil
		}
		return h.recordAudit(ctx, tx, now, auditEvent{BillingAccountID: accountID, Kind: AuditSubscriptionResumed, ActorUserID: userID})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// CreateTopUp buys credit with a one-time charge. Owner and admin only. The
// credit arrives later with the verified payment webhook.
func (h *CreditHandler) CreateTopUp(ctx context.Context, req *connect.Request[accountsv1.CreateTopUpRequest]) (*connect.Response[accountsv1.CreateTopUpResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	key, err := normalizeID("idempotency_key", req.Msg.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	amount := req.Msg.GetAmountUsdMicros()
	if err := billing.ValidateTopUpAmount(amount); err != nil {
		return nil, err
	}
	authorize := func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		return require(account, PermissionManage)
	}
	created := func(tx pgx.Tx, topUpID string) error {
		return h.recordAudit(ctx, tx, h.timestamp(), auditEvent{
			BillingAccountID: accountID, Kind: AuditTopUpRequested, ActorUserID: userID,
			Details: map[string]string{"top_up_id": topUpID, "amount_usd_micros": strconv.FormatInt(amount, 10)},
		})
	}
	err = h.billing.CreateTopUp(ctx, accountID, userID, key, amount, authorize, created)
	if err != nil {
		return nil, h.internal(ctx, "create_top_up", err)
	}
	return connect.NewResponse(&accountsv1.CreateTopUpResponse{}), nil
}

// GetAutoTopUpSettings reads the automatic top-up settings. Every member may
// call it.
func (h *CreditHandler) GetAutoTopUpSettings(ctx context.Context, req *connect.Request[accountsv1.GetAutoTopUpSettingsRequest]) (*connect.Response[accountsv1.GetAutoTopUpSettingsResponse], error) {
	accountID, err := h.read(ctx, req.Msg.GetBillingAccountId(), PermissionRead)
	if err != nil {
		return nil, h.internal(ctx, "get_auto_top_up_settings", err)
	}
	settings, err := h.autoTopUp(ctx, accountID)
	if err != nil {
		return nil, h.internal(ctx, "get_auto_top_up_settings", err)
	}
	return connect.NewResponse(&accountsv1.GetAutoTopUpSettingsResponse{Settings: settings}), nil
}

func (h *CreditHandler) autoTopUp(ctx context.Context, accountID string) (*accountsv1.AutoTopUpSettings, error) {
	st, err := billing.LoadSettings(ctx, h.db.Pool, accountID)
	if err != nil {
		return nil, err
	}
	mtd, err := billing.MonthToDate(ctx, h.db.Pool, accountID, h.timestamp())
	if err != nil {
		return nil, err
	}
	return st.AutoTopUpProto(mtd), nil
}

// UpdateAutoTopUpSettings replaces the automatic top-up settings. Owner and
// admin only.
func (h *CreditHandler) UpdateAutoTopUpSettings(ctx context.Context, req *connect.Request[accountsv1.UpdateAutoTopUpSettingsRequest]) (*connect.Response[accountsv1.UpdateAutoTopUpSettingsResponse], error) {
	if _, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId()); err != nil {
		return nil, err
	}
	if err := billing.ValidateAutoTopUp(req.Msg.GetSettings()); err != nil {
		return nil, err
	}
	var accountID string
	err := h.change(ctx, "update_auto_top_up_settings", req.Msg.GetBillingAccountId(), func(tx pgx.Tx, id, userID string, now time.Time) error {
		accountID = id
		in := req.Msg.GetSettings()
		if _, err := billing.UpdateAutoTopUp(ctx, tx, id, in, now); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: id, Kind: AuditAutoTopUpSettingsUpdated, ActorUserID: userID,
			Details: map[string]string{
				"enabled":                strconv.FormatBool(in.GetEnabled()),
				"threshold_usd_micros":   strconv.FormatInt(in.GetThresholdUsdMicros(), 10),
				"amount_usd_micros":      strconv.FormatInt(in.GetAmountUsdMicros(), 10),
				"monthly_cap_usd_micros": strconv.FormatInt(in.GetMonthlyCapUsdMicros(), 10),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	settings, err := h.autoTopUp(ctx, accountID)
	if err != nil {
		return nil, h.internal(ctx, "update_auto_top_up_settings", err)
	}
	return connect.NewResponse(&accountsv1.UpdateAutoTopUpSettingsResponse{Settings: settings}), nil
}

// GetAlertSettings reads the low-balance alert threshold. Every member may
// call it.
func (h *CreditHandler) GetAlertSettings(ctx context.Context, req *connect.Request[accountsv1.GetAlertSettingsRequest]) (*connect.Response[accountsv1.GetAlertSettingsResponse], error) {
	accountID, err := h.read(ctx, req.Msg.GetBillingAccountId(), PermissionRead)
	if err != nil {
		return nil, h.internal(ctx, "get_alert_settings", err)
	}
	st, err := billing.LoadSettings(ctx, h.db.Pool, accountID)
	if err != nil {
		return nil, h.internal(ctx, "get_alert_settings", err)
	}
	return connect.NewResponse(&accountsv1.GetAlertSettingsResponse{Settings: st.AlertProto()}), nil
}

// UpdateAlertSettings replaces the low-balance alert threshold. Owner and
// admin only.
func (h *CreditHandler) UpdateAlertSettings(ctx context.Context, req *connect.Request[accountsv1.UpdateAlertSettingsRequest]) (*connect.Response[accountsv1.UpdateAlertSettingsResponse], error) {
	if _, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId()); err != nil {
		return nil, err
	}
	in := req.Msg.GetSettings()
	if in == nil {
		return nil, invalidArgument("settings is required")
	}
	if v := in.GetLowBalanceThresholdUsdMicros(); v < 0 || !billing.WholeCents(v) {
		return nil, invalidArgument("low_balance_threshold_usd_micros must be non-negative whole cents")
	}
	var st billing.Settings
	err := h.change(ctx, "update_alert_settings", req.Msg.GetBillingAccountId(), func(tx pgx.Tx, accountID, userID string, now time.Time) error {
		var err error
		if st, err = billing.UpdateAlert(ctx, tx, accountID, in, now); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditAlertSettingsUpdated, ActorUserID: userID,
			Details: map[string]string{"low_balance_threshold_usd_micros": strconv.FormatInt(in.GetLowBalanceThresholdUsdMicros(), 10)},
		})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&accountsv1.UpdateAlertSettingsResponse{Settings: st.AlertProto()}), nil
}

// ListInvoices lists the completed Paddle transactions, newest first. Owner
// and admin only, because invoices carry the payer's address.
func (h *CreditHandler) ListInvoices(ctx context.Context, req *connect.Request[accountsv1.ListInvoicesRequest]) (*connect.Response[accountsv1.ListInvoicesResponse], error) {
	const rpc = "ListInvoices"
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, accountID)
	if err != nil {
		return nil, err
	}
	var afterTime time.Time
	if tok.After != "" {
		if tok.After, err = normalizeID("page_token", tok.After); err != nil || tok.AfterTime == 0 {
			return nil, invalidArgument("page_token is not valid for this request")
		}
		afterTime = time.UnixMicro(tok.AfterTime).UTC()
	}
	if _, err := h.read(ctx, accountID, PermissionManage); err != nil {
		return nil, h.internal(ctx, "list_invoices", err)
	}
	invoices, err := billing.ListInvoices(ctx, h.db.Pool, accountID, afterTime, tok.After, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_invoices", err)
	}
	resp := &accountsv1.ListInvoicesResponse{}
	if len(invoices) > size {
		invoices = invoices[:size]
		last := invoices[size-1]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: accountID, After: last.CreditGrantID, AfterTime: last.CreateTime.UnixMicro()})
	}
	for _, i := range invoices {
		resp.Invoices = append(resp.Invoices, i.Proto())
	}
	return connect.NewResponse(resp), nil
}

// maxPaddleIDBytes bounds a Paddle ID in a request.
const maxPaddleIDBytes = 64

// paddleTransactionID checks a Paddle transaction ID: `txn_` followed by
// lowercase letters and digits.
func paddleTransactionID(value string) (string, error) {
	body, ok := strings.CutPrefix(value, "txn_")
	if !ok || body == "" || len(value) > maxPaddleIDBytes {
		return "", invalidArgument("paddle_transaction_id must be a Paddle transaction ID")
	}
	for _, c := range body {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", invalidArgument("paddle_transaction_id must be a Paddle transaction ID")
		}
	}
	return value, nil
}

// GetInvoiceUrl returns a short-lived link to the Paddle invoice PDF of one
// transaction. Owner and admin only. The link is never logged.
func (h *CreditHandler) GetInvoiceUrl(ctx context.Context, req *connect.Request[accountsv1.GetInvoiceUrlRequest]) (*connect.Response[accountsv1.GetInvoiceUrlResponse], error) {
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	txnID, err := paddleTransactionID(req.Msg.GetPaddleTransactionId())
	if err != nil {
		return nil, err
	}
	if _, err := h.read(ctx, accountID, PermissionManage); err != nil {
		return nil, h.internal(ctx, "get_invoice_url", err)
	}
	link, err := h.billing.InvoiceURL(ctx, h.db.Pool, accountID, txnID)
	if err != nil {
		return nil, h.internal(ctx, "get_invoice_url", err)
	}
	return connect.NewResponse(&accountsv1.GetInvoiceUrlResponse{InvoiceUrl: link}), nil
}
