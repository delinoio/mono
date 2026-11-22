// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// UsageHandler implements UsageService, the service API of the consuming
// Delino services. The authentication interceptor admits only
// machine-to-machine tokens with the scope `accounts:report-usage`; the
// handler checks that the client may use the named service.
type UsageHandler struct {
	*Service
}

var _ accountsv1connect.UsageServiceHandler = (*UsageHandler)(nil)

// Limits of the usage API (docs/protos-delino-accounts-api-contract.md).
const (
	maxReportsPerRequest      = 500
	maxOrganizationIDBytes    = 128
	maxOrganizationNameBytes  = 200
	maxWindowEndAheadOfServer = time.Minute
)

// usageClient returns the machine-to-machine client of the call and checks
// that it may use service. A client may use only the services that
// USAGE_SERVICE_CLIENTS maps it to.
func (s *Service) usageClient(ctx context.Context, services ...accountsv1.ServiceId) (string, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok || caller.Kind != auth.TokenKindMachineToMachine || caller.Token.ClientID == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("missing access token"))
	}
	allowed := s.usageClients[caller.Token.ClientID]
	if len(allowed) == 0 {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "usage_client_not_mapped", slog.String("client_id", caller.Token.ClientID))
		return "", permissionDenied("the client may not use the usage API")
	}
	for _, svc := range services {
		if !slices.Contains(allowed, svc) {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "usage_client_service_denied",
				slog.String("client_id", caller.Token.ClientID), slog.String("service", svc.String()))
			return "", permissionDenied("the client may not use the service " + svc.String())
		}
	}
	return caller.Token.ClientID, nil
}

// serviceField checks a required ServiceId and returns its stable
// identifier.
func serviceField(field string, s accountsv1.ServiceId) (string, error) {
	key, ok := billing.ServiceKey(s)
	if !ok {
		return "", invalidArgument(field + " must be a known service")
	}
	return key, nil
}

// organizationIDField applies the organization ID rule: 1 to 128 bytes of
// printable ASCII.
func organizationIDField(field, value string) (string, error) {
	if value == "" || len(value) > maxOrganizationIDBytes {
		return "", invalidArgument(field + " must have 1 to 128 bytes")
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c > 0x7e {
			return "", invalidArgument(field + " must be printable ASCII")
		}
	}
	return value, nil
}

// organizationNameField applies the organization name rule: at most 200
// bytes of UTF-8 without control characters; it may be empty.
func organizationNameField(value string) (string, error) {
	if len(value) > maxOrganizationNameBytes || !utf8.ValidString(value) {
		return "", invalidArgument("organization_name must have at most 200 bytes of UTF-8")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", invalidArgument("organization_name must not contain control characters")
		}
	}
	return value, nil
}

// validateReport checks one usage report and returns it with stable
// identifiers.
func validateReport(r *accountsv1.UsageReport, now time.Time) (billing.UsageReport, error) {
	if r == nil {
		return billing.UsageReport{}, errors.New("is missing")
	}
	service, ok := billing.ServiceKey(r.GetService())
	if !ok {
		return billing.UsageReport{}, errors.New("service must be a known service")
	}
	if _, err := organizationIDField("organization_id", r.GetOrganizationId()); err != nil {
		return billing.UsageReport{}, errors.New("organization_id must have 1 to 128 bytes of printable ASCII")
	}
	meter, meterService, ok := billing.MeterKey(r.GetMeter())
	if !ok {
		return billing.UsageReport{}, errors.New("meter must be a known meter")
	}
	if meterService != r.GetService() {
		return billing.UsageReport{}, errors.New("meter must belong to the service")
	}
	ws := r.GetWindowStart()
	if ws == nil || ws.CheckValid() != nil {
		return billing.UsageReport{}, errors.New("window_start is required")
	}
	if ws.GetNanos() != 0 || ws.GetSeconds()%int64(billing.WindowLength/time.Second) != 0 {
		return billing.UsageReport{}, errors.New("window_start must be aligned to 5 minutes")
	}
	start := ws.AsTime().UTC()
	if start.Add(billing.WindowLength).After(now.Add(maxWindowEndAheadOfServer)) {
		return billing.UsageReport{}, errors.New("the window must not end more than 1 minute after the server time")
	}
	if r.GetQuantity() < 0 {
		return billing.UsageReport{}, errors.New("quantity must not be negative")
	}
	if r.GetAmountUsdMicros() < 0 {
		return billing.UsageReport{}, errors.New("amount_usd_micros must not be negative")
	}
	return billing.UsageReport{
		Service: service, OrganizationID: r.GetOrganizationId(), Meter: meter, WindowStart: start,
		Quantity: r.GetQuantity(), Amount: r.GetAmountUsdMicros(),
	}, nil
}

// ReportUsage validates every report before it applies any, then applies
// them in request order, each in its own transaction, so a retry of a
// request that failed half-way is safe.
func (h *UsageHandler) ReportUsage(ctx context.Context, req *connect.Request[accountsv1.ReportUsageRequest]) (*connect.Response[accountsv1.ReportUsageResponse], error) {
	in := req.Msg.GetReports()
	if len(in) == 0 || len(in) > maxReportsPerRequest {
		return nil, invalidArgument("reports must hold 1 to 500 reports")
	}
	now := h.timestamp()
	reports := make([]billing.UsageReport, len(in))
	var services []accountsv1.ServiceId
	for i, r := range in {
		report, err := validateReport(r, now)
		if err != nil {
			return nil, invalidArgument(fmt.Sprintf("reports[%d]: %v", i, err))
		}
		reports[i] = report
		if !slices.Contains(services, r.GetService()) {
			services = append(services, r.GetService())
		}
	}
	if _, err := h.usageClient(ctx, services...); err != nil {
		return nil, err
	}
	resp := &accountsv1.ReportUsageResponse{Results: make([]*accountsv1.UsageReportResult, len(in))}
	named := map[string][]string{}
	for i, r := range reports {
		res, err := h.billing.ReportUsage(ctx, r)
		if err != nil {
			return nil, h.internal(ctx, "report_usage", err)
		}
		resp.Results[i] = &accountsv1.UsageReportResult{
			Service:          in[i].GetService(),
			OrganizationId:   r.OrganizationID,
			Meter:            in[i].GetMeter(),
			WindowStart:      timestamppb.New(r.WindowStart),
			Duplicate:        res.Duplicate,
			Unattributed:     res.Unattributed,
			BillingAccountId: res.BillingAccountID,
			LedgerEntryId:    res.LedgerEntryID,
		}
		if !slices.Contains(named[r.Service], r.OrganizationID) {
			named[r.Service] = append(named[r.Service], r.OrganizationID)
		}
	}
	keys := make([]string, 0, len(named))
	for service := range named {
		keys = append(keys, service)
	}
	slices.Sort(keys)
	for _, service := range keys {
		statuses, err := h.billing.CurrentStatuses(ctx, service, named[service])
		if err != nil {
			return nil, h.internal(ctx, "report_usage", err)
		}
		for _, st := range statuses {
			resp.Statuses = append(resp.Statuses, st.Proto())
		}
	}
	return connect.NewResponse(resp), nil
}

// ListBillingStatusChanges returns the current statuses of the service's
// linked organizations whose sequence is greater than the cursor.
func (h *UsageHandler) ListBillingStatusChanges(ctx context.Context, req *connect.Request[accountsv1.ListBillingStatusChangesRequest]) (*connect.Response[accountsv1.ListBillingStatusChangesResponse], error) {
	service, err := serviceField("service", req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	after := req.Msg.GetAfterSequence()
	if after < 0 {
		return nil, invalidArgument("after_sequence must not be negative")
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	if _, err := h.usageClient(ctx, req.Msg.GetService()); err != nil {
		return nil, err
	}
	statuses, err := h.billing.ListStatusChanges(ctx, service, after, size)
	if err != nil {
		return nil, h.internal(ctx, "list_billing_status_changes", err)
	}
	resp := &accountsv1.ListBillingStatusChangesResponse{NextSequence: after}
	for _, st := range statuses {
		resp.Statuses = append(resp.Statuses, st.Proto())
		resp.NextSequence = st.Sequence
	}
	return connect.NewResponse(resp), nil
}

// LinkOrganization links an organization of the service to a billing
// account for a user who is owner or admin of it.
func (h *UsageHandler) LinkOrganization(ctx context.Context, req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
	m := req.Msg
	service, err := serviceField("service", m.GetService())
	if err != nil {
		return nil, err
	}
	orgID, err := organizationIDField("organization_id", m.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", m.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	userID, err := userIDField(m.GetUserId())
	if err != nil {
		return nil, err
	}
	name, err := organizationNameField(m.GetOrganizationName())
	if err != nil {
		return nil, err
	}
	if _, err := h.usageClient(ctx, m.GetService()); err != nil {
		return nil, err
	}
	now := h.timestamp()
	var account billingAccount
	var status billing.OrganizationStatus
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err = loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		if err := h.link(ctx, tx, service, orgID, accountID, userID, name, now); err != nil {
			return err
		}
		// The status of a new link, or a suspension of an existing link
		// that became due, is recorded before the response reads it.
		if err := h.billing.SyncStatuses(ctx, tx, accountID); err != nil {
			return err
		}
		st, ok, err := billing.CurrentStatus(ctx, tx, service, orgID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the new link has no status")
		}
		status = st
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "link_organization", err)
	}
	return connect.NewResponse(&accountsv1.LinkOrganizationResponse{Status: status.Proto(), BillingAccountName: account.Name}), nil
}

// link creates the link of the organization to the billing account, which
// tx holds locked, or changes the name of an existing link to it. A link to
// another billing account is already_exists.
func (h *UsageHandler) link(ctx context.Context, tx pgx.Tx, service, orgID, accountID, userID, name string, now time.Time) error {
	var linkID, linkedAccount, oldName string
	err := tx.QueryRow(ctx, `SELECT organization_link_id::text, billing_account_id::text, organization_name
		FROM organization_links WHERE service = $1 AND organization_id = $2 AND unlink_time IS NULL FOR UPDATE`,
		service, orgID).Scan(&linkID, &linkedAccount, &oldName)
	switch {
	case err == nil && linkedAccount != accountID:
		return alreadyExists("the organization is linked to another billing account")
	case err == nil:
		if oldName == name {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE organization_links SET organization_name = $2 WHERE organization_link_id = $1`,
			linkID, name); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditOrganizationLinkRenamed, ActorUserID: userID,
			Details: map[string]string{"service": service, "organization_id": orgID, "old_name": oldName, "new_name": name},
		})
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if linkID, err = newID(); err != nil {
		return err
	}
	// The billing account lock is held; take the organization lock after it
	// (see billing.LockOrganization) so an unattributed report of the
	// organization either commits before this link is inserted or finds it.
	if err := billing.LockOrganization(ctx, tx, service, orgID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_links (organization_link_id, service, organization_id,
			billing_account_id, organization_name, link_time) VALUES ($1, $2, $3, $4, $5, $6)`,
		linkID, service, orgID, accountID, name, now); err != nil {
		if db.IsUniqueViolation(err) {
			// A concurrent link of the organization to another billing
			// account committed first; links to this billing account
			// serialize on its lock.
			return alreadyExists("the organization is linked to another billing account")
		}
		return err
	}
	h.logger.LogAttrs(ctx, slog.LevelInfo, "billing_organization_linked",
		slog.String("billing_account_id", accountID), slog.String("service", service),
		slog.String("organization_id", orgID), slog.String("actor_user_id", userID))
	return h.recordAudit(ctx, tx, now, auditEvent{
		BillingAccountID: accountID, Kind: AuditOrganizationLinked, ActorUserID: userID,
		Details: map[string]string{"service": service, "organization_id": orgID, "organization_name": name},
	})
}

// UnlinkOrganization removes the current link of an organization. An
// organization without a link succeeds and changes nothing.
func (h *UsageHandler) UnlinkOrganization(ctx context.Context, req *connect.Request[accountsv1.UnlinkOrganizationRequest]) (*connect.Response[accountsv1.UnlinkOrganizationResponse], error) {
	service, err := serviceField("service", req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	orgID, err := organizationIDField("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	clientID, err := h.usageClient(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	current, ok, err := billing.CurrentStatus(ctx, h.db.Pool, service, orgID)
	if err != nil {
		return nil, h.internal(ctx, "unlink_organization", err)
	}
	if !ok {
		return connect.NewResponse(&accountsv1.UnlinkOrganizationResponse{}), nil
	}
	now := h.timestamp()
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		// The billing account lock comes first, as for every change of a
		// billing account; the link row is locked after it.
		if err := billing.LockAccount(ctx, tx, current.BillingAccountID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE organization_links SET unlink_time = GREATEST($2, link_time)
			WHERE organization_link_id = $1 AND unlink_time IS NULL`, current.LinkID, now)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		h.logger.LogAttrs(ctx, slog.LevelInfo, "billing_organization_unlinked",
			slog.String("billing_account_id", current.BillingAccountID), slog.String("service", service),
			slog.String("organization_id", orgID), slog.String("client_id", clientID))
		// The calling service names no user; the actor is its
		// machine-to-machine application.
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: current.BillingAccountID, Kind: AuditOrganizationUnlinked, ActorUserID: clientID,
			Details: map[string]string{"service": service, "organization_id": orgID, "actor_kind": "machine_to_machine"},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "unlink_organization", err)
	}
	return connect.NewResponse(&accountsv1.UnlinkOrganizationResponse{}), nil
}

// ListManageableBillingAccounts lists the billing accounts in which the user
// is owner or admin, by billing account ID.
func (h *UsageHandler) ListManageableBillingAccounts(ctx context.Context, req *connect.Request[accountsv1.ListManageableBillingAccountsRequest]) (*connect.Response[accountsv1.ListManageableBillingAccountsResponse], error) {
	const rpc = "ListManageableBillingAccounts"
	userID, err := userIDField(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, userID)
	if err != nil {
		return nil, err
	}
	after := "00000000-0000-0000-0000-000000000000"
	if tok.After != "" {
		if after, err = normalizeID("page_token", tok.After); err != nil {
			return nil, invalidArgument("page_token is not valid for this request")
		}
	}
	if _, err := h.usageClient(ctx); err != nil {
		return nil, err
	}
	rows, err := h.db.Pool.Query(ctx, `SELECT a.billing_account_id::text, a.name, a.create_time, a.balance_usd_micros, m.role
		FROM billing_account_members m
		JOIN billing_accounts a ON a.billing_account_id = m.billing_account_id
		WHERE m.user_id = $1 AND m.role IN ('owner', 'admin') AND a.delete_time IS NULL AND a.billing_account_id > $2
		ORDER BY a.billing_account_id
		LIMIT $3`, userID, after, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_manageable_billing_accounts", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (billingAccount, error) {
		var a billingAccount
		var role string
		err := row.Scan(&a.ID, &a.Name, &a.CreateTime, &a.Balance, &role)
		a.CallerRole = Role(role)
		return a, err
	})
	if err != nil {
		return nil, h.internal(ctx, "list_manageable_billing_accounts", err)
	}
	resp := &accountsv1.ListManageableBillingAccountsResponse{}
	if len(accounts) > size {
		accounts = accounts[:size]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: userID, After: accounts[size-1].ID})
	}
	for _, a := range accounts {
		resp.BillingAccounts = append(resp.BillingAccounts, a.proto())
	}
	return connect.NewResponse(resp), nil
}

// GetOrganizationBilling reads the billing account, balance, status and
// current-month usage of a linked organization.
func (h *UsageHandler) GetOrganizationBilling(ctx context.Context, req *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error) {
	service, err := serviceField("service", req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	orgID, err := organizationIDField("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, err := h.usageClient(ctx, req.Msg.GetService()); err != nil {
		return nil, err
	}
	st, ok, err := h.billing.FreshStatus(ctx, service, orgID)
	if err != nil {
		return nil, h.internal(ctx, "get_organization_billing", err)
	}
	if !ok {
		return nil, notFound("the organization has no link")
	}
	now := h.timestamp()
	var name string
	if err := h.db.Pool.QueryRow(ctx, `SELECT name FROM billing_accounts WHERE billing_account_id = $1`,
		st.BillingAccountID).Scan(&name); err != nil {
		return nil, h.internal(ctx, "get_organization_billing", err)
	}
	balance, _, err := billing.Balance(ctx, h.db.Pool, st.BillingAccountID, now)
	if err != nil {
		return nil, h.internal(ctx, "get_organization_billing", err)
	}
	monthStart := billing.MonthStart(now)
	usage, err := billing.OrganizationMonthUsage(ctx, h.db.Pool, service, orgID, monthStart, monthStart.AddDate(0, 1, 0))
	if err != nil {
		return nil, h.internal(ctx, "get_organization_billing", err)
	}
	resp := &accountsv1.GetOrganizationBillingResponse{
		BillingAccountId:   st.BillingAccountID,
		BillingAccountName: name,
		BalanceUsdMicros:   balance,
		Status:             st.Proto(),
		MonthStart:         timestamppb.New(monthStart),
	}
	for _, meter := range billing.ServiceMeters(req.Msg.GetService()) {
		key, _, _ := billing.MeterKey(meter)
		u := usage[key]
		resp.MeterUsages = append(resp.MeterUsages, &accountsv1.MeterUsage{Meter: meter, Quantity: u.Quantity, AmountUsdMicros: u.Amount})
	}
	return connect.NewResponse(resp), nil
}

// BillingUsageHandler implements BillingUsageService for the members of a
// billing account.
type BillingUsageHandler struct {
	*Service
}

var _ accountsv1connect.BillingUsageServiceHandler = (*BillingUsageHandler)(nil)

// member checks the caller's membership in the billing account; every
// member may read usage.
func (h *BillingUsageHandler) member(ctx context.Context, accountID string) error {
	userID, err := callerUserID(ctx)
	if err != nil {
		return err
	}
	account, err := loadAccess(ctx, h.db.Pool, accountID, userID, lockNone)
	if err != nil {
		return err
	}
	return require(account, PermissionRead)
}

// ListLinkedOrganizations lists the organizations that are linked to the
// billing account now, in link order.
func (h *BillingUsageHandler) ListLinkedOrganizations(ctx context.Context, req *connect.Request[accountsv1.ListLinkedOrganizationsRequest]) (*connect.Response[accountsv1.ListLinkedOrganizationsResponse], error) {
	const rpc = "ListLinkedOrganizations"
	if _, err := callerUserID(ctx); err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, accountID)
	if err != nil {
		return nil, err
	}
	afterTime := time.Time{}
	afterID := "00000000-0000-0000-0000-000000000000"
	if tok.After != "" {
		if afterID, err = normalizeID("page_token", tok.After); err != nil || tok.AfterTime == 0 {
			return nil, invalidArgument("page_token is not valid for this request")
		}
		afterTime = time.UnixMicro(tok.AfterTime).UTC()
	}
	if err := h.member(ctx, accountID); err != nil {
		return nil, h.internal(ctx, "list_linked_organizations", err)
	}
	if err := h.billing.SuspendDue(ctx, billing.SuspendFilter{BillingAccountIDs: []string{accountID}}); err != nil {
		return nil, h.internal(ctx, "list_linked_organizations", err)
	}
	statuses, err := billing.LinkedOrganizations(ctx, h.db.Pool, accountID, afterTime, afterID, tok.After != "", size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_linked_organizations", err)
	}
	resp := &accountsv1.ListLinkedOrganizationsResponse{}
	if len(statuses) > size {
		statuses = statuses[:size]
		last := statuses[size-1]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: accountID, After: last.LinkID, AfterTime: last.LinkTime.UnixMicro()})
	}
	for _, st := range statuses {
		service, _ := billing.ServiceFromKey(st.Service)
		resp.LinkedOrganizations = append(resp.LinkedOrganizations, &accountsv1.LinkedOrganization{
			Service:          service,
			OrganizationId:   st.OrganizationID,
			OrganizationName: st.OrganizationName,
			LinkTime:         timestamppb.New(st.LinkTime),
			Status:           st.Proto(),
		})
	}
	return connect.NewResponse(resp), nil
}

// Calendar limits of ListUsageSummary.
const (
	minSummaryYear = 2000
	maxSummaryYear = 9999
)

// summaryKeySeparator separates the parts of a summary page position.
// Organization IDs are printable ASCII and never contain it.
const summaryKeySeparator = "\n"

// ListUsageSummary summarizes the usage charged to the billing account in
// one UTC calendar month by service, organization and meter.
func (h *BillingUsageHandler) ListUsageSummary(ctx context.Context, req *connect.Request[accountsv1.ListUsageSummaryRequest]) (*connect.Response[accountsv1.ListUsageSummaryResponse], error) {
	const rpc = "ListUsageSummary"
	if _, err := callerUserID(ctx); err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	year, month := req.Msg.GetYear(), req.Msg.GetMonth()
	if year < minSummaryYear || year > maxSummaryYear {
		return nil, invalidArgument("year must be 2000 to 9999")
	}
	if month < 1 || month > 12 {
		return nil, invalidArgument("month must be 1 to 12")
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	// The token is bound to the billing account and the month.
	scope := fmt.Sprintf("%s/%04d-%02d", accountID, year, month)
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, scope)
	if err != nil {
		return nil, err
	}
	var after *billing.SummaryKey
	if tok.After != "" {
		parts := strings.Split(tok.After, summaryKeySeparator)
		if len(parts) != 3 {
			return nil, invalidArgument("page_token is not valid for this request")
		}
		after = &billing.SummaryKey{Service: parts[0], OrganizationID: parts[1], Meter: parts[2]}
	}
	if err := h.member(ctx, accountID); err != nil {
		return nil, h.internal(ctx, "list_usage_summary", err)
	}
	from := time.Date(int(year), time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	rows, total, err := billing.UsageSummary(ctx, h.db.Pool, accountID, from, from.AddDate(0, 1, 0), after, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_usage_summary", err)
	}
	resp := &accountsv1.ListUsageSummaryResponse{TotalAmountUsdMicros: total}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: scope,
			After: strings.Join([]string{last.Service, last.OrganizationID, last.Meter}, summaryKeySeparator)})
	}
	for _, r := range rows {
		service, _ := billing.ServiceFromKey(r.Service)
		meter, _ := billing.MeterFromKey(r.Meter)
		resp.Rows = append(resp.Rows, &accountsv1.UsageSummaryRow{
			Service: service, OrganizationId: r.OrganizationID, Meter: meter, Quantity: r.Quantity, AmountUsdMicros: r.Amount,
		})
	}
	return connect.NewResponse(resp), nil
}
