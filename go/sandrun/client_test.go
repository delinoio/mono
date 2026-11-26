// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// newTestClient starts a fake server and returns a client with short
// delays and one organization.
func newTestClient(t *testing.T, cred string) (*Client, *sandruntest.Server, string) {
	t.Helper()
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("test").GetOrganizationId()
	if cred == "" {
		cred = srv.AccessToken
	}
	c, err := New(Options{BaseURL: srv.URL, APIKey: cred})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = timing{backoffMin: 5 * time.Millisecond, backoffMax: 20 * time.Millisecond, retryBudget: 2 * time.Second, waitInterval: 5 * time.Millisecond}
	return c, srv, org
}

func TestNewValidatesOptions(t *testing.T) {
	for _, base := range []string{"ftp://x", "https://x/path", "https://x?q", "https://u@x", "https://x#f"} {
		if _, err := New(Options{BaseURL: base, APIKey: "k"}); err == nil {
			t.Errorf("New(%q) accepted an invalid origin", base)
		}
	}
	if _, err := New(Options{}); !errors.Is(err, ErrNoCredential) {
		t.Errorf("New without credential: %v", err)
	}
	if _, err := New(Options{APIKey: "a", TokenSource: func(context.Context) (string, error) { return "b", nil }}); err == nil {
		t.Error("New accepted two credentials")
	}
	c, err := New(Options{APIKey: "k"})
	if err != nil || c.BaseURL() != DefaultBaseURL {
		t.Errorf("default base URL: %v %q", err, c.BaseURL())
	}
}

func TestTokenSourceAndErrorMapping(t *testing.T) {
	srv := sandruntest.New()
	defer srv.Close()
	calls := 0
	var logs bytes.Buffer
	c, err := New(Options{BaseURL: srv.URL, Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), TokenSource: func(context.Context) (string, error) {
		calls++
		return srv.AccessToken, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = c.Organizations().GetOrganization(ctx, connect.NewRequest(&sandrunv1.GetOrganizationRequest{OrganizationId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want not_found", connect.CodeOf(err))
	}
	if calls != 1 || srv.LastCredential() != sandruntest.CredentialAccessToken {
		t.Fatalf("token source calls %d, credential %v", calls, srv.LastCredential())
	}
	if strings.Contains(logs.String(), srv.AccessToken) || !strings.Contains(logs.String(), "not_found") {
		t.Fatalf("logs: %s", logs.String())
	}

	// A server message that echoes the token is redacted, and the code
	// stays.
	srv.Fail("/delino.sandrun.v1.OrganizationService/ListOrganizations", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad token %s", srv.AccessToken)))
	_, err = c.Organizations().ListOrganizations(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), srv.AccessToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redacted error: %v", err)
	}
	if !connect.IsWireError(err) {
		t.Fatal("the redacted error lost its wire flag")
	}

	// A token source failure never reaches the server.
	failing, _ := New(Options{BaseURL: srv.URL, TokenSource: func(context.Context) (string, error) { return "", errors.New("signed out") }})
	if _, err := failing.Organizations().ListOrganizations(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{})); err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatalf("token source error: %v", err)
	}
}

func TestAPIKeyOnlyOnDevSessionService(t *testing.T) {
	c, srv, org := newTestClient(t, sandruntest.DefaultAPIKey)
	ctx := context.Background()
	_, err := c.Organizations().GetOrganization(ctx, connect.NewRequest(&sandrunv1.GetOrganizationRequest{OrganizationId: org}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want permission_denied", connect.CodeOf(err))
	}
	if _, err := c.DevSessions().ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: org})); err != nil {
		t.Fatal(err)
	}
	if srv.LastCredential() != sandruntest.CredentialAPIKey || !c.UsesAPIKey() {
		t.Fatal("the API key was not used")
	}
}

func TestIssuedPersonalAPIKey(t *testing.T) {
	admin, srv, org := newTestClient(t, "")
	ctx := t.Context()
	issue := func(expiresAt *timestamppb.Timestamp) (*Client, string) {
		t.Helper()
		res, err := admin.APIKeys().CreateApiKey(ctx, connect.NewRequest(&sandrunv1.CreateApiKeyRequest{
			OrganizationId: org, Name: "dev session", ExpiresAt: expiresAt,
		}))
		if err != nil {
			t.Fatal(err)
		}
		client, err := New(Options{BaseURL: srv.URL, APIKey: res.Msg.GetSecret()})
		if err != nil {
			t.Fatal(err)
		}
		return client, res.Msg.GetApiKey().GetApiKeyId()
	}
	list := func(client *Client, orgID string) error {
		_, err := client.DevSessions().ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: orgID}))
		return err
	}

	key, keyID := issue(nil)
	if err := list(key, org); err != nil {
		t.Fatalf("list with issued key: %v", err)
	}
	if srv.LastCredential() != sandruntest.CredentialAPIKey {
		t.Fatal("the issued key was not recorded as an API key")
	}
	_, err := key.Organizations().GetOrganization(ctx, connect.NewRequest(&sandrunv1.GetOrganizationRequest{OrganizationId: org}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("get organization with issued key: %v, want permission_denied", err)
	}
	otherOrg := srv.AddOrganization("other").GetOrganizationId()
	if err := list(key, otherOrg); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("list with issued key from another organization: %v, want not_found", err)
	}

	t.Run("stream in another organization", func(t *testing.T) {
		profile := srv.AddDevSessionProfile(otherOrg, "test", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
		session, err := admin.DevSessions().CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
			OrganizationId: otherOrg, RunnerProfileId: profile.GetRunnerProfileId(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		read := func(client *Client) error {
			stream, err := client.DevSessions().ReadDevSessionFile(ctx, connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
				OrganizationId: otherOrg, DevSessionId: session.Msg.GetDevSession().GetDevSessionId(),
			}))
			if err != nil {
				return err
			}
			defer stream.Close()
			stream.Receive()
			return stream.Err()
		}
		if err := read(admin); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("stream with access token and missing read ID: %v, want invalid_argument", err)
		}
		if err := read(key); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("stream with issued key from another organization: %v, want not_found", err)
		}
	})

	_, err = admin.APIKeys().RevokeApiKey(ctx, connect.NewRequest(&sandrunv1.RevokeApiKeyRequest{
		OrganizationId: org, ApiKeyId: keyID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := list(key, org); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("list with revoked key: %v, want unauthenticated", err)
	}

	now := time.Now()
	unexpired, _ := issue(timestamppb.New(now.Add(time.Hour)))
	if err := list(unexpired, org); err != nil {
		t.Fatalf("list with unexpired key: %v", err)
	}
	expired, _ := issue(timestamppb.New(now.Add(-time.Hour)))
	if err := list(expired, org); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("list with expired key: %v, want unauthenticated", err)
	}

	unknown, err := New(Options{BaseURL: srv.URL, APIKey: "sandrun_secret-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if err := list(unknown, org); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("list with unknown key: %v, want unauthenticated", err)
	}
}

func TestPagination(t *testing.T) {
	c, srv, _ := newTestClient(t, "")
	for i := range 119 {
		srv.AddOrganization(fmt.Sprintf("org-%d", i))
	}
	orgs, err := Collect(c.AllOrganizations(context.Background(), &sandrunv1.ListOrganizationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 120 {
		t.Fatalf("got %d organizations, want 120", len(orgs))
	}
	if n := srv.Calls("/delino.sandrun.v1.OrganizationService/ListOrganizations"); n != 3 {
		t.Fatalf("ListOrganizations calls = %d, want 3", n)
	}
	// An early break stops paging.
	for range c.AllOrganizations(context.Background(), &sandrunv1.ListOrganizationsRequest{PageSize: 10}) {
		break
	}
	if n := srv.Calls("/delino.sandrun.v1.OrganizationService/ListOrganizations"); n != 4 {
		t.Fatalf("calls after break = %d, want 4", n)
	}
	// An error ends the iteration.
	srv.Fail("/delino.sandrun.v1.OrganizationService/ListOrganizations", nil, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	_, err = Collect(c.AllOrganizations(context.Background(), &sandrunv1.ListOrganizationsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("pagination error = %v", err)
	}
}

func TestPaginationConcurrentNilRequest(t *testing.T) {
	var mu sync.Mutex
	tokenCounts := make(map[string]int)
	handler := connect.NewUnaryHandler(sandrunv1connect.OrganizationServiceListOrganizationsProcedure,
		func(_ context.Context, req *connect.Request[sandrunv1.ListOrganizationsRequest]) (*connect.Response[sandrunv1.ListOrganizationsResponse], error) {
			token := req.Msg.GetPageToken()
			mu.Lock()
			tokenCounts[token]++
			mu.Unlock()

			var id, next string
			switch token {
			case "":
				id, next = "org-1", "next"
			case "next":
				id = "org-2"
			default:
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected page token %q", token))
			}
			return connect.NewResponse(&sandrunv1.ListOrganizationsResponse{
				Organizations: []*sandrunv1.Organization{{OrganizationId: id}},
				NextPageToken: next,
			}), nil
		})
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	client, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: sandruntest.DefaultAccessToken})
	if err != nil {
		t.Fatal(err)
	}

	seq := client.AllOrganizations(t.Context(), nil)
	start := make(chan struct{})
	var traversals sync.WaitGroup
	for i := range 2 {
		traversals.Go(func() {
			<-start
			orgs, err := Collect(seq)
			if err != nil {
				t.Errorf("traversal %d: Collect: %v", i, err)
				return
			}
			var gotIDs []string
			for _, org := range orgs {
				gotIDs = append(gotIDs, org.GetOrganizationId())
			}
			wantIDs := []string{"org-1", "org-2"}
			t.Logf("traversal %d: collected organization IDs = %q", i, gotIDs)
			if !slices.Equal(gotIDs, wantIDs) {
				t.Errorf("traversal %d: got %q, want %q", i, gotIDs, wantIDs)
			}
		})
	}
	close(start)
	traversals.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("request counts by token = %v", tokenCounts)
	if len(tokenCounts) != 2 || tokenCounts[""] != 2 || tokenCounts["next"] != 2 {
		t.Errorf("request counts by token = %v, want two requests each for the empty token and next", tokenCounts)
	}
}

func TestFakePageSizeValidation(t *testing.T) {
	c, srv, _ := newTestClient(t, "")
	for i := range 200 {
		srv.AddOrganization(fmt.Sprintf("org-%d", i))
	}
	ctx := t.Context()
	list := c.Organizations().ListOrganizations

	if _, err := list(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{PageSize: -1})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("negative page size: got %v, want invalid_argument", err)
	}
	if _, err := list(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{PageSize: 1, PageToken: "malformed"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("malformed page token: got %v, want invalid_argument", err)
	}

	first, err := list(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(first.Msg.GetOrganizations()); got != 50 {
		t.Fatalf("default page size: got %d, want 50", got)
	}
	token := first.Msg.GetNextPageToken()
	if token == "" {
		t.Fatal("default page is missing a continuation token")
	}
	next, err := list(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{PageToken: token}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(next.Msg.GetOrganizations()); got != 50 {
		t.Fatalf("next page size: got %d, want 50", got)
	}
	firstIDs := make(map[string]bool, 50)
	for _, org := range first.Msg.GetOrganizations() {
		firstIDs[org.GetOrganizationId()] = true
	}
	for _, org := range next.Msg.GetOrganizations() {
		if firstIDs[org.GetOrganizationId()] {
			t.Fatalf("next page repeated organization %q", org.GetOrganizationId())
		}
	}

	capped, err := list(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{PageSize: 201}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(capped.Msg.GetOrganizations()); got != 200 {
		t.Fatalf("capped page size: got %d, want 200", got)
	}
}

func TestPaginationRepeatedStartingToken(t *testing.T) {
	const loopError = "sandrun: the server returned the page token of an earlier page again"
	const pageSize = 10
	for _, tc := range []struct {
		name       string
		nextTokens map[string]string
		wantTokens []string
		wantIDs    []string
		wantError  string
	}{
		{
			name:       "starting token repeats immediately",
			nextTokens: map[string]string{"resume": "resume"},
			wantTokens: []string{"resume"},
			wantIDs:    []string{"org-1"},
			wantError:  loopError,
		},
		{
			name:       "cycle returns to starting token",
			nextTokens: map[string]string{"resume": "later", "later": "resume"},
			wantTokens: []string{"resume", "later"},
			wantIDs:    []string{"org-1", "org-2"},
			wantError:  loopError,
		},
		{
			name:       "distinct continuation ends with empty token",
			nextTokens: map[string]string{"resume": "later", "later": ""},
			wantTokens: []string{"resume", "later"},
			wantIDs:    []string{"org-1", "org-2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var tokens []string
			ids := map[string]string{"resume": "org-1", "later": "org-2"}
			handler := connect.NewUnaryHandler(sandrunv1connect.OrganizationServiceListOrganizationsProcedure,
				func(_ context.Context, req *connect.Request[sandrunv1.ListOrganizationsRequest]) (*connect.Response[sandrunv1.ListOrganizationsResponse], error) {
					token := req.Msg.GetPageToken()
					if got := req.Msg.GetPageSize(); got != pageSize {
						t.Errorf("page size for token %q = %d, want %d", token, got, pageSize)
					}
					mu.Lock()
					tokens = append(tokens, token)
					mu.Unlock()
					next, ok := tc.nextTokens[token]
					if !ok {
						return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected page token %q", token))
					}
					return connect.NewResponse(&sandrunv1.ListOrganizationsResponse{
						Organizations: []*sandrunv1.Organization{{OrganizationId: ids[token]}},
						NextPageToken: next,
					}), nil
				})
			srv := httptest.NewTLSServer(handler)
			t.Cleanup(srv.Close)
			client, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: sandruntest.DefaultAccessToken})
			if err != nil {
				t.Fatal(err)
			}

			req := &sandrunv1.ListOrganizationsRequest{PageToken: "resume", PageSize: pageSize}
			orgs, err := Collect(client.AllOrganizations(t.Context(), req))
			var gotIDs []string
			for _, org := range orgs {
				gotIDs = append(gotIDs, org.GetOrganizationId())
			}
			mu.Lock()
			gotTokens := slices.Clone(tokens)
			mu.Unlock()
			t.Logf("requested tokens = %q, collected organization IDs = %q, error = %v", gotTokens, gotIDs, err)
			if !slices.Equal(gotTokens, tc.wantTokens) {
				t.Errorf("requested tokens = %q, want %q", gotTokens, tc.wantTokens)
			}
			if !slices.Equal(gotIDs, tc.wantIDs) {
				t.Errorf("collected organization IDs = %q, want %q", gotIDs, tc.wantIDs)
			}
			var gotError string
			if err != nil {
				gotError = err.Error()
			}
			if gotError != tc.wantError {
				t.Errorf("Collect error = %q, want %q", gotError, tc.wantError)
			}
			if req.GetPageToken() != "resume" {
				t.Errorf("caller request token = %q, want resume", req.GetPageToken())
			}
			if req.GetPageSize() != pageSize {
				t.Errorf("caller request page size = %d, want %d", req.GetPageSize(), pageSize)
			}
		})
	}
}

func TestWatchQueueReconnects(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	srv.AddRequest(&sandrunv1.Request{RequestId: "r1", OrganizationId: org, State: sandrunv1.RequestState_REQUEST_STATE_QUEUED})
	srv.WatchEventLimit = 2 // the request and the snapshot end
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var kinds []QueueEventKind
	err := c.WatchQueue(ctx, org, func(ev QueueEvent) error {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == QueueEventGap && ev.Err != nil {
			t.Errorf("gap after a normal end has error %v", ev.Err)
		}
		if len(kinds) >= 8 {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("WatchQueue = %v, want the handler error", err)
	}
	want := []QueueEventKind{QueueEventRequest, QueueEventSnapshotComplete, QueueEventGap, QueueEventRequest, QueueEventSnapshotComplete, QueueEventGap, QueueEventRequest, QueueEventSnapshotComplete}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}

	// A transient failure is retried with a gap that carries the error; a
	// terminal one ends the watch.
	srv.Fail("/delino.sandrun.v1.WorkService/WatchQueue", connect.NewError(connect.CodeUnavailable, errors.New("restart")))
	var gapErr error
	_ = c.WatchQueue(ctx, org, func(ev QueueEvent) error {
		if ev.Kind == QueueEventGap && gapErr == nil {
			gapErr = ev.Err
		}
		if ev.Kind == QueueEventSnapshotComplete {
			return errors.New("stop")
		}
		return nil
	})
	if connect.CodeOf(gapErr) != connect.CodeUnavailable {
		t.Fatalf("gap error = %v", gapErr)
	}
	err = c.WatchQueue(ctx, "missing-org", func(QueueEvent) error { return nil })
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("terminal watch error = %v", err)
	}
}

func TestBackoffBounds(t *testing.T) {
	b := backoff{min: time.Second, max: 30 * time.Second}
	prev := time.Duration(0)
	for i := range 10 {
		d := b.next()
		if d > 30*time.Second || d < 800*time.Millisecond {
			t.Fatalf("delay %d = %v out of bounds", i, d)
		}
		if i < 4 && d < prev {
			t.Fatalf("delay %d = %v below %v", i, d, prev)
		}
		prev = d
	}
	b.reset()
	if d := b.next(); d > time.Second {
		t.Fatalf("after reset %v", d)
	}
}

func TestWaitDevSession(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	prof := srv.AddDevSessionProfile(org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
	srv.NewSessionState = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	srv.PromoteAfterGets = 3
	ctx := context.Background()
	res, err := c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.GetDevSession().GetDevSessionId()
	ds, err := c.WaitDevSession(ctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if err != nil || ds.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("wait = %v %v", ds.GetState(), err)
	}

	srv.PromoteAfterGets = 0
	res, _ = c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
	id = res.Msg.GetDevSession().GetDevSessionId()
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	ds, err = c.WaitDevSession(wctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if !errors.Is(err, context.DeadlineExceeded) || ds.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("wait past deadline = %v %v", ds.GetState(), err)
	}

	// A terminal session ends the wait.
	srv.SetSessionState(id, sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
	ds, err = c.WaitDevSession(ctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if err != nil || ds.GetState() != sandrunv1.RequestState_REQUEST_STATE_EXPIRED {
		t.Fatalf("wait terminal = %v %v", ds.GetState(), err)
	}
	if !IsTerminalState(sandrunv1.RequestState_REQUEST_STATE_FAILED) || IsTerminalState(sandrunv1.RequestState_REQUEST_STATE_RUNNING) {
		t.Fatal("IsTerminalState")
	}
}
