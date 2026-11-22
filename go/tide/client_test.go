// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
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
