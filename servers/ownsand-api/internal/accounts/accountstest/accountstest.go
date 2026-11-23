// SPDX-License-Identifier: FSL-1.1-ALv2

// Package accountstest is an in-memory fake of the delino-accounts API for
// the control plane tests: a Logto token endpoint for the client credentials
// grant and the UsageService with billing accounts, organization links,
// billing statuses with a change sequence and usage reports. It follows the
// rules of docs/protos-delino-accounts-api-contract.md that the control
// plane depends on; it is not a second implementation of accounts-api.
package accountstest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts"
)

// Credentials of the fake machine-to-machine application.
const (
	ClientID     = "fake-ownsand-m2m"
	ClientSecret = "fake-ownsand-m2m-secret"
	accessToken  = "fake-m2m-access-token"
)

// Method names for Fail and Calls.
const (
	MethodReportUsage                   = "ReportUsage"
	MethodListBillingStatusChanges      = "ListBillingStatusChanges"
	MethodLinkOrganization              = "LinkOrganization"
	MethodUnlinkOrganization            = "UnlinkOrganization"
	MethodListManageableBillingAccounts = "ListManageableBillingAccounts"
	MethodGetOrganizationBilling        = "GetOrganizationBilling"
)

type account struct {
	id, name     string
	members      map[string]accountsv1.BillingAccountRole
	balance      int64
	status       accountsv1.BillingStatus
	reason       accountsv1.BillingStatusReason
	blockedSince time.Time
}

type link struct {
	account string
	name    string
	since   time.Time
	status  *accountsv1.OrganizationBillingStatus
}

// usageKey identifies the usage of one organization and meter in one UTC
// calendar month. The accounts API counts a report in the month in which its
// window starts, so the month is part of the key.
type usageKey struct {
	org   string
	meter accountsv1.Meter
	month time.Time // first instant of the UTC month of the window start
}

// utcMonth returns the first instant of the UTC calendar month of t.
func utcMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Server is a running fake delino-accounts API.
type Server struct {
	// URL is the API origin and TokenEndpoint the Logto token endpoint.
	URL           string
	TokenEndpoint string

	srv *httptest.Server
	now func() time.Time

	mu       sync.Mutex
	seq      int64
	accounts map[string]*account
	links    map[string]*link
	usage    map[usageKey][2]int64
	reported map[string]bool
	failures map[string]error
	// replyFailures fail a call after it took effect: a lost response.
	replyFailures map[string]error
	gates         map[string]*Gate
	calls         map[string]int
}

// Gate holds the calls of one method before they take effect, until
// Release. Tests use it to keep a call in flight while other operations run.
type Gate struct {
	// Entered receives one value for each call that reached the gate.
	Entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// Release lets the held calls, and every later call, continue.
func (g *Gate) Release() { g.once.Do(func() { close(g.release) }) }

// New starts a fake. now is its clock; nil selects time.Now.
func New(t testing.TB, now func() time.Time) *Server {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	f := &Server{now: now, accounts: map[string]*account{}, links: map[string]*link{}, usage: map[usageKey][2]int64{},
		reported: map[string]bool{}, failures: map[string]error{}, replyFailures: map[string]error{}, gates: map[string]*Gate{}, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oidc/token", f.token)
	path, h := accountsv1connect.NewUsageServiceHandler(handler{f}, connect.WithInterceptors(authCheck{}))
	mux.Handle(path, h)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	f.TokenEndpoint = f.srv.URL + "/oidc/token"
	return f
}

// Client returns a UsageService client that authenticates like the control
// plane: with a client credentials token from the fake token endpoint.
func (f *Server) Client() accountsv1connect.UsageServiceClient {
	tokens := accounts.NewTokenSource(accounts.TokenConfig{Endpoint: f.TokenEndpoint, ClientID: ClientID, ClientSecret: ClientSecret, HTTPClient: f.srv.Client()})
	return accounts.NewUsageClient(f.srv.Client(), f.URL, tokens)
}

// Env returns the control plane environment variables that point usage
// reporting and billing at the fake.
func (f *Server) Env() []string {
	return []string{
		"ACCOUNTS_API_ORIGIN=" + f.URL,
		"LOGTO_TOKEN_ENDPOINT=" + f.TokenEndpoint,
		"ACCOUNTS_M2M_CLIENT_ID=" + ClientID,
		"ACCOUNTS_M2M_CLIENT_SECRET=" + ClientSecret,
	}
}

func (f *Server) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != ClientID || secret != ClientSecret {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken, "token_type": "Bearer", "expires_in": 3600})
}

// authCheck rejects requests without the fake access token.
type authCheck struct{}

func (authCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Header().Get("Authorization") != "Bearer "+accessToken {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing machine-to-machine token"))
		}
		return next(ctx, req)
	}
}

func (authCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (authCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// AddAccount adds a billing account in good standing (ALLOWED) with the
// given member roles and a balance of $10.
func (f *Server) AddAccount(id, name string, members map[string]accountsv1.BillingAccountRole) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[id] = &account{id: id, name: name, members: members, balance: 10_000_000,
		status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, reason: accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NONE}
}

// SetAccountStatus changes the status of a billing account and of every
// organization that is linked to it, each with a new sequence. blockedSince
// is the start of the non-payment of the account; it is ignored for
// ALLOWED.
func (f *Server) SetAccountStatus(id string, status accountsv1.BillingStatus, blockedSince time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[id]
	a.status, a.blockedSince = status, blockedSince
	a.reason = accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NONE
	if status != accountsv1.BillingStatus_BILLING_STATUS_ALLOWED {
		a.reason = accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NON_POSITIVE_BALANCE
	}
	orgs := make([]string, 0, len(f.links))
	for org, l := range f.links {
		if l.account == id {
			orgs = append(orgs, org)
		}
	}
	sort.Strings(orgs)
	for _, org := range orgs {
		f.refreshLocked(org)
	}
}

// refreshLocked recomputes the status of a linked organization with a new
// sequence.
func (f *Server) refreshLocked(org string) {
	l := f.links[org]
	a := f.accounts[l.account]
	f.seq++
	st := &accountsv1.OrganizationBillingStatus{Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: org, BillingAccountId: a.id,
		Status: a.status, Reason: a.reason, Sequence: f.seq}
	if a.status != accountsv1.BillingStatus_BILLING_STATUS_ALLOWED {
		start := a.blockedSince
		if l.since.After(start) {
			start = l.since
		}
		st.BlockedSince = timestamppb.New(start)
		st.SuspendTime = timestamppb.New(start.Add(72 * time.Hour))
	}
	l.status = st
}

// Fail makes every later call of method fail with err; nil clears it.
func (f *Server) Fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failures, method)
		return
	}
	f.failures[method] = err
}

// FailReply makes every later call of method take effect and then fail with
// err, like a response that got lost; nil clears it. It applies to
// LinkOrganization and UnlinkOrganization.
func (f *Server) FailReply(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.replyFailures, method)
		return
	}
	f.replyFailures[method] = err
}

// Hold makes every later call of method wait before it takes effect until
// the returned gate is released.
func (f *Server) Hold(method string) *Gate {
	g := &Gate{Entered: make(chan struct{}, 16), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[method] = g
	return g
}

// Calls returns the number of calls of method, failed calls included.
func (f *Server) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// Link returns the billing account that the organization is linked to and
// the organization name that the last LinkOrganization sent.
func (f *Server) Link(org string) (accountID, orgName string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[org]
	if !ok {
		return "", "", false
	}
	return l.account, l.name, true
}

// Status returns the current status of a linked organization.
func (f *Server) Status(org string) *accountsv1.OrganizationBillingStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.links[org]; ok {
		return proto.Clone(l.status).(*accountsv1.OrganizationBillingStatus)
	}
	return nil
}

// begin counts a call and returns the configured failure.
func (f *Server) begin(method string) error {
	f.mu.Lock()
	f.calls[method]++
	err := f.failures[method]
	g := f.gates[method]
	f.mu.Unlock()
	if g != nil {
		g.Entered <- struct{}{}
		<-g.release
	}
	return err
}

type handler struct{ f *Server }

func (h handler) ReportUsage(_ context.Context, req *connect.Request[accountsv1.ReportUsageRequest]) (*connect.Response[accountsv1.ReportUsageResponse], error) {
	f := h.f
	if err := f.begin(MethodReportUsage); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &accountsv1.ReportUsageResponse{}
	named := map[string]bool{}
	for _, r := range req.Msg.GetReports() {
		key := r.GetOrganizationId() + "/" + r.GetMeter().String() + "/" + r.GetWindowStart().AsTime().String()
		res := &accountsv1.UsageReportResult{Service: r.GetService(), OrganizationId: r.GetOrganizationId(), Meter: r.GetMeter(), WindowStart: r.GetWindowStart(),
			Duplicate: f.reported[key]}
		if !f.reported[key] {
			f.reported[key] = true
			k := usageKey{org: r.GetOrganizationId(), meter: r.GetMeter(), month: utcMonth(r.GetWindowStart().AsTime())}
			u := f.usage[k]
			f.usage[k] = [2]int64{u[0] + r.GetQuantity(), u[1] + r.GetAmountUsdMicros()}
			if l, ok := f.links[r.GetOrganizationId()]; ok {
				res.BillingAccountId = l.account
				f.accounts[l.account].balance -= r.GetAmountUsdMicros()
			} else {
				res.Unattributed = true
			}
		}
		resp.Results = append(resp.Results, res)
		named[r.GetOrganizationId()] = true
	}
	orgs := make([]string, 0, len(named))
	for org := range named {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	for _, org := range orgs {
		if l, ok := f.links[org]; ok {
			resp.Statuses = append(resp.Statuses, proto.Clone(l.status).(*accountsv1.OrganizationBillingStatus))
		}
	}
	return connect.NewResponse(resp), nil
}

func (h handler) ListBillingStatusChanges(_ context.Context, req *connect.Request[accountsv1.ListBillingStatusChangesRequest]) (*connect.Response[accountsv1.ListBillingStatusChangesResponse], error) {
	f := h.f
	if err := f.begin(MethodListBillingStatusChanges); err != nil {
		return nil, err
	}
	if req.Msg.GetAfterSequence() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("negative after_sequence"))
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 {
		size = 50
	}
	size = min(size, 200)
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*accountsv1.OrganizationBillingStatus
	for _, l := range f.links {
		if l.status.GetSequence() > req.Msg.GetAfterSequence() {
			out = append(out, proto.Clone(l.status).(*accountsv1.OrganizationBillingStatus))
		}
	}
	slices.SortFunc(out, func(a, b *accountsv1.OrganizationBillingStatus) int { return int(a.GetSequence() - b.GetSequence()) })
	if len(out) > size {
		out = out[:size]
	}
	next := req.Msg.GetAfterSequence()
	if n := len(out); n > 0 {
		next = out[n-1].GetSequence()
	}
	return connect.NewResponse(&accountsv1.ListBillingStatusChangesResponse{Statuses: out, NextSequence: next}), nil
}

func (h handler) LinkOrganization(_ context.Context, req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
	f := h.f
	if err := f.begin(MethodLinkOrganization); err != nil {
		return nil, err
	}
	m := req.Msg
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[m.GetBillingAccountId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("billing account not found"))
	}
	role, member := a.members[m.GetUserId()]
	if !member {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("billing account not found"))
	}
	if role != accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER && role != accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("the user must be owner or admin of the billing account"))
	}
	if l, ok := f.links[m.GetOrganizationId()]; ok {
		if l.account != a.id {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("the organization is linked to another billing account"))
		}
		l.name = m.GetOrganizationName()
	} else {
		f.links[m.GetOrganizationId()] = &link{account: a.id, name: m.GetOrganizationName(), since: f.now().UTC()}
		f.refreshLocked(m.GetOrganizationId())
	}
	if err := f.replyFailures[MethodLinkOrganization]; err != nil {
		return nil, err
	}
	return connect.NewResponse(&accountsv1.LinkOrganizationResponse{Status: proto.Clone(f.links[m.GetOrganizationId()].status).(*accountsv1.OrganizationBillingStatus),
		BillingAccountName: a.name}), nil
}

func (h handler) UnlinkOrganization(_ context.Context, req *connect.Request[accountsv1.UnlinkOrganizationRequest]) (*connect.Response[accountsv1.UnlinkOrganizationResponse], error) {
	f := h.f
	if err := f.begin(MethodUnlinkOrganization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	delete(f.links, req.Msg.GetOrganizationId())
	err := f.replyFailures[MethodUnlinkOrganization]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&accountsv1.UnlinkOrganizationResponse{}), nil
}

// manageablePageToken is the content of a ListManageableBillingAccounts page
// token. Like the token of accounts-api it is opaque to clients, remembers the
// sort position (the last account ID returned) and is bound to the RPC and to
// the user that listed, so a raw account ID or another user's token is
// rejected.
type manageablePageToken struct {
	RPC   string `json:"rpc"`
	Scope string `json:"scope"`
	After string `json:"after"`
}

// errInvalidPageToken is the error of accounts-api for a page token that is
// malformed or does not belong to the request.
func errInvalidPageToken() error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New("page_token is not valid for this request"))
}

func encodeManageablePageToken(userID, after string) string {
	raw, _ := json.Marshal(manageablePageToken{RPC: MethodListManageableBillingAccounts, Scope: userID, After: after})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeManageablePageToken returns the account ID after which the page of
// userID resumes; an empty token is the first page.
func decodeManageablePageToken(userID, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", errInvalidPageToken()
	}
	var t manageablePageToken
	if err := json.Unmarshal(raw, &t); err != nil || t.RPC != MethodListManageableBillingAccounts || t.Scope != userID || t.After == "" {
		return "", errInvalidPageToken()
	}
	return t.After, nil
}

func (h handler) ListManageableBillingAccounts(_ context.Context, req *connect.Request[accountsv1.ListManageableBillingAccountsRequest]) (*connect.Response[accountsv1.ListManageableBillingAccountsResponse], error) {
	f := h.f
	if err := f.begin(MethodListManageableBillingAccounts); err != nil {
		return nil, err
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 {
		size = 50
	}
	userID := req.Msg.GetUserId()
	after, err := decodeManageablePageToken(userID, req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*accountsv1.BillingAccount
	for _, a := range f.accounts {
		role := a.members[userID]
		if (role == accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER || role == accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN) && a.id > after {
			out = append(out, &accountsv1.BillingAccount{BillingAccountId: a.id, Name: a.name, CallerRole: role})
		}
	}
	slices.SortFunc(out, func(a, b *accountsv1.BillingAccount) int {
		switch {
		case a.GetBillingAccountId() < b.GetBillingAccountId():
			return -1
		case a.GetBillingAccountId() > b.GetBillingAccountId():
			return 1
		}
		return 0
	})
	next := ""
	if len(out) > size {
		out = out[:size]
		next = encodeManageablePageToken(userID, out[size-1].GetBillingAccountId())
	}
	return connect.NewResponse(&accountsv1.ListManageableBillingAccountsResponse{BillingAccounts: out, NextPageToken: next}), nil
}

func (h handler) GetOrganizationBilling(_ context.Context, req *connect.Request[accountsv1.GetOrganizationBillingRequest]) (*connect.Response[accountsv1.GetOrganizationBillingResponse], error) {
	f := h.f
	if err := f.begin(MethodGetOrganizationBilling); err != nil {
		return nil, err
	}
	org := req.Msg.GetOrganizationId()
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[org]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the organization has no link"))
	}
	a := f.accounts[l.account]
	now := f.now().UTC()
	resp := &accountsv1.GetOrganizationBillingResponse{BillingAccountId: a.id, BillingAccountName: a.name, BalanceUsdMicros: a.balance,
		Status: proto.Clone(l.status).(*accountsv1.OrganizationBillingStatus), MonthStart: timestamppb.New(utcMonth(now))}
	for _, m := range []accountsv1.Meter{accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS, accountsv1.Meter_METER_OWNSAND_R2_STORAGE,
		accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC} {
		u := f.usage[usageKey{org: org, meter: m, month: utcMonth(now)}]
		resp.MeterUsages = append(resp.MeterUsages, &accountsv1.MeterUsage{Meter: m, Quantity: u[0], AmountUsdMicros: u[1]})
	}
	return connect.NewResponse(resp), nil
}
