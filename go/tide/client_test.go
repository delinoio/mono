// SPDX-License-Identifier: Apache-2.0

package tide

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

	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

// newTestClient starts a fake server and returns a client with short
// delays and one organization.
func newTestClient(t *testing.T, cred string) (*Client, *tidetest.Server, string) {
	t.Helper()
	srv := tidetest.New()
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
	srv := tidetest.New()
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
	_, err = c.Organizations().GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want not_found", connect.CodeOf(err))
	}
	if calls != 1 || srv.LastCredential() != tidetest.CredentialAccessToken {
		t.Fatalf("token source calls %d, credential %v", calls, srv.LastCredential())
	}
	if strings.Contains(logs.String(), srv.AccessToken) || !strings.Contains(logs.String(), "not_found") {
		t.Fatalf("logs: %s", logs.String())
	}

	// A server message that echoes the token is redacted, and the code
	// stays.
	srv.Fail("/delino.tide.v1.OrganizationService/ListOrganizations", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad token %s", srv.AccessToken)))
	_, err = c.Organizations().ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), srv.AccessToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redacted error: %v", err)
	}
	if !connect.IsWireError(err) {
		t.Fatal("the redacted error lost its wire flag")
	}

	// A token source failure never reaches the server.
	failing, _ := New(Options{BaseURL: srv.URL, TokenSource: func(context.Context) (string, error) { return "", errors.New("signed out") }})
	if _, err := failing.Organizations().ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{})); err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatalf("token source error: %v", err)
	}
}

func TestAPIKeyOnlyOnDevSessionService(t *testing.T) {
	c, srv, org := newTestClient(t, tidetest.DefaultAPIKey)
	ctx := context.Background()
	_, err := c.Organizations().GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want permission_denied", connect.CodeOf(err))
	}
	if _, err := c.DevSessions().ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: org})); err != nil {
		t.Fatal(err)
	}
	if srv.LastCredential() != tidetest.CredentialAPIKey || !c.UsesAPIKey() {
		t.Fatal("the API key was not used")
	}
}

func TestIssuedPersonalAPIKey(t *testing.T) {
	admin, srv, org := newTestClient(t, "")
	ctx := t.Context()
	issue := func(expiresAt *timestamppb.Timestamp) (*Client, string) {
		t.Helper()
		res, err := admin.APIKeys().CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{
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
		_, err := client.DevSessions().ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: orgID}))
		return err
	}

	key, keyID := issue(nil)
	if err := list(key, org); err != nil {
		t.Fatalf("list with issued key: %v", err)
	}
	if srv.LastCredential() != tidetest.CredentialAPIKey {
		t.Fatal("the issued key was not recorded as an API key")
	}
	_, err := key.Organizations().GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("get organization with issued key: %v, want permission_denied", err)
	}
	otherOrg := srv.AddOrganization("other").GetOrganizationId()
	if err := list(key, otherOrg); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("list with issued key from another organization: %v, want not_found", err)
	}

	t.Run("stream in another organization", func(t *testing.T) {
		profile := srv.AddDevSessionProfile(otherOrg, "test", tidev1.GuestOs_GUEST_OS_LINUX, t.TempDir())
		session, err := admin.DevSessions().CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{
			OrganizationId: otherOrg, RunnerProfileId: profile.GetRunnerProfileId(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		read := func(client *Client) error {
			stream, err := client.DevSessions().ReadDevSessionFile(ctx, connect.NewRequest(&tidev1.ReadDevSessionFileRequest{
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

	_, err = admin.APIKeys().RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{
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

	unknown, err := New(Options{BaseURL: srv.URL, APIKey: "tide_secret-unknown"})
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
	orgs, err := Collect(c.AllOrganizations(context.Background(), &tidev1.ListOrganizationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 120 {
		t.Fatalf("got %d organizations, want 120", len(orgs))
	}
	if n := srv.Calls("/delino.tide.v1.OrganizationService/ListOrganizations"); n != 3 {
		t.Fatalf("ListOrganizations calls = %d, want 3", n)
	}
	// An early break stops paging.
	for range c.AllOrganizations(context.Background(), &tidev1.ListOrganizationsRequest{PageSize: 10}) {
		break
	}
	if n := srv.Calls("/delino.tide.v1.OrganizationService/ListOrganizations"); n != 4 {
		t.Fatalf("calls after break = %d, want 4", n)
	}
	// An error ends the iteration.
	srv.Fail("/delino.tide.v1.OrganizationService/ListOrganizations", nil, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	_, err = Collect(c.AllOrganizations(context.Background(), &tidev1.ListOrganizationsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("pagination error = %v", err)
	}
}

func TestPaginationRepeatedStartingToken(t *testing.T) {
	const loopError = "tide: the server returned the page token of an earlier page again"
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
			handler := connect.NewUnaryHandler(tidev1connect.OrganizationServiceListOrganizationsProcedure,
				func(_ context.Context, req *connect.Request[tidev1.ListOrganizationsRequest]) (*connect.Response[tidev1.ListOrganizationsResponse], error) {
					token := req.Msg.GetPageToken()
					mu.Lock()
					tokens = append(tokens, token)
					mu.Unlock()
					next, ok := tc.nextTokens[token]
					if !ok {
						return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected page token %q", token))
					}
					return connect.NewResponse(&tidev1.ListOrganizationsResponse{
						Organizations: []*tidev1.Organization{{OrganizationId: ids[token]}},
						NextPageToken: next,
					}), nil
				})
			srv := httptest.NewTLSServer(handler)
			t.Cleanup(srv.Close)
			client, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: tidetest.DefaultAccessToken})
			if err != nil {
				t.Fatal(err)
			}

			req := &tidev1.ListOrganizationsRequest{PageToken: "resume"}
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
		})
	}
}

func TestWatchQueueReconnects(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	srv.AddRequest(&tidev1.Request{RequestId: "r1", OrganizationId: org, State: tidev1.RequestState_REQUEST_STATE_QUEUED})
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
	srv.Fail("/delino.tide.v1.WorkService/WatchQueue", connect.NewError(connect.CodeUnavailable, errors.New("restart")))
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
	prof := srv.AddDevSessionProfile(org, "linux", tidev1.GuestOs_GUEST_OS_LINUX, t.TempDir())
	srv.NewSessionState = tidev1.RequestState_REQUEST_STATE_QUEUED
	srv.PromoteAfterGets = 3
	ctx := context.Background()
	res, err := c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.GetDevSession().GetDevSessionId()
	ds, err := c.WaitDevSession(ctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if err != nil || ds.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("wait = %v %v", ds.GetState(), err)
	}

	srv.PromoteAfterGets = 0
	res, _ = c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
	id = res.Msg.GetDevSession().GetDevSessionId()
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	ds, err = c.WaitDevSession(wctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if !errors.Is(err, context.DeadlineExceeded) || ds.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("wait past deadline = %v %v", ds.GetState(), err)
	}

	// A terminal session ends the wait.
	srv.SetSessionState(id, tidev1.RequestState_REQUEST_STATE_EXPIRED)
	ds, err = c.WaitDevSession(ctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if err != nil || ds.GetState() != tidev1.RequestState_REQUEST_STATE_EXPIRED {
		t.Fatalf("wait terminal = %v %v", ds.GetState(), err)
	}
	if !IsTerminalState(tidev1.RequestState_REQUEST_STATE_FAILED) || IsTerminalState(tidev1.RequestState_REQUEST_STATE_RUNNING) {
		t.Fatal("IsTerminalState")
	}
}
