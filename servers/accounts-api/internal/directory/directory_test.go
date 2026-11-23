// SPDX-License-Identifier: FSL-1.1-ALv2

package directory_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
)

func TestLogtoUser(t *testing.T) {
	var tokens, rejectFirst atomic.Int32
	rejectFirst.Store(1)
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oidc/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "m2m" || secret != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("resource") != srvURL+"/api" || r.Form.Get("scope") != "all" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": 3600})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/u1":
			if rejectFirst.CompareAndSwap(1, 0) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer tok2" && r.Header.Get("Authorization") != "Bearer tok1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "primaryEmail": "A@Example.com", "name": "Ann", "username": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/u2":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u2", "primaryEmail": nil, "name": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	l := directory.NewLogto(directory.LogtoConfig{
		Endpoint: srv.URL + "/", ClientID: "m2m", ClientSecret: "s3cret",
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ctx := context.Background()
	user, err := l.User(ctx, "u1")
	if err != nil {
		t.Fatalf("User(u1) error = %v", err)
	}
	if user.PrimaryEmail != "A@Example.com" || user.Name != "Ann" {
		t.Fatalf("User(u1) = %+v", user)
	}
	if tokens.Load() != 2 {
		t.Fatalf("token requests = %d, want 2 (one renewal after 401)", tokens.Load())
	}
	user, err = l.User(ctx, "u2")
	if err != nil || user != (directory.User{}) {
		t.Fatalf("User(u2) = %+v, %v", user, err)
	}
	if tokens.Load() != 2 {
		t.Fatalf("the cached token was not reused: %d requests", tokens.Load())
	}
	if _, err := l.User(ctx, "missing"); !errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User(missing) error = %v, want ErrUserNotFound", err)
	}
	if _, err := l.User(ctx, "broken"); err == nil || errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User(broken) error = %v, want an unavailable error", err)
	}
}

func TestLogtoTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	l := directory.NewLogto(directory.LogtoConfig{Endpoint: srv.URL, ClientID: "m2m", ClientSecret: "wrong"})
	if _, err := l.User(context.Background(), "u1"); err == nil || errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User() error = %v, want a token error", err)
	}
}

// stalledTokenServer answers token requests only after release is closed and
// reports each token request that has started on started.
type stalledTokenServer struct {
	srv     *httptest.Server
	started chan struct{}
	release chan struct{}
	tokens  atomic.Int32
	once    sync.Once
}

func newStalledTokenServer(t *testing.T) *stalledTokenServer {
	t.Helper()
	s := &stalledTokenServer{started: make(chan struct{}, 8), release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oidc/token":
			s.tokens.Add(1)
			s.started <- struct{}{}
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case r.URL.Path == "/api/users/u1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "primaryEmail": "a@example.com", "name": "Ann"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		s.unblock()
		s.srv.Close()
	})
	return s
}

func (s *stalledTokenServer) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *stalledTokenServer) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("token request did not start")
	}
}

func (s *stalledTokenServer) client(now func() time.Time) *directory.Logto {
	return directory.NewLogto(directory.LogtoConfig{
		Endpoint: s.srv.URL, ClientID: "m2m", ClientSecret: "s3cret", Now: now,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
}

type userResult struct {
	user directory.User
	err  error
}

func startUser(ctx context.Context, l *directory.Logto) <-chan userResult {
	ch := make(chan userResult, 1)
	go func() {
		user, err := l.User(ctx, "u1")
		ch <- userResult{user: user, err: err}
	}()
	return ch
}

func receive(t *testing.T, ch <-chan userResult, what string) userResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return userResult{}
	}
}

func TestLogtoCanceledWaiterReturnsWhileRefreshRuns(t *testing.T) {
	s := newStalledTokenServer(t)
	l := s.client(nil)

	leader := startUser(context.Background(), l)
	s.waitStarted(t)

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiter := startUser(waiterCtx, l)
	cancelWaiter()
	if r := receive(t, waiter, "canceled waiter"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context.Canceled", r.err)
	}

	// The refresh of the leader is still running and finishes normally.
	select {
	case r := <-leader:
		t.Fatalf("leader returned early: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	s.unblock()
	if r := receive(t, leader, "leader"); r.err != nil || r.user.Name != "Ann" {
		t.Fatalf("leader = %+v, want the user", r)
	}
	if got := s.tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1", got)
	}
}

func TestLogtoCanceledLeaderDoesNotAbortSharedRefresh(t *testing.T) {
	s := newStalledTokenServer(t)
	l := s.client(nil)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := startUser(leaderCtx, l)
	s.waitStarted(t)
	waiter := startUser(context.Background(), l)

	cancelLeader()
	if r := receive(t, leader, "canceled leader"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", r.err)
	}
	s.unblock()
	if r := receive(t, waiter, "waiter"); r.err != nil || r.user.Name != "Ann" {
		t.Fatalf("waiter = %+v, want the user from the shared refresh", r)
	}
	if got := s.tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1 (the refresh must be shared)", got)
	}
}

func TestLogtoCachedTokenIsReusedUntilExpiry(t *testing.T) {
	s := newStalledTokenServer(t)
	s.unblock()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	l := s.client(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	for range 2 {
		if _, err := l.User(context.Background(), "u1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1 while the token is valid", got)
	}
	// 3600 s lifetime minus the one-minute refresh margin.
	mu.Lock()
	now = now.Add(time.Hour - 30*time.Second)
	mu.Unlock()
	if _, err := l.User(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if got := s.tokens.Load(); got != 2 {
		t.Fatalf("token requests = %d, want 2 after the token entered the refresh margin", got)
	}
}

// waitWaiters waits until n callers have started or joined the running token
// refresh.
func waitWaiters(t *testing.T, l *directory.Logto, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.RefreshWaiters() < n {
		if time.Now().After(deadline) {
			t.Fatalf("refresh waiters = %d, want %d", l.RefreshWaiters(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLogtoFailedRefreshIsSharedByJoinedCallers(t *testing.T) {
	var tokens atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc/token":
			// The first request is held and fails; later ones succeed.
			if tokens.Add(1) == 1 {
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/api/users/u1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "primaryEmail": "a@example.com", "name": "Ann"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	defer unblock()
	l := directory.NewLogto(directory.LogtoConfig{
		Endpoint: srv.URL, ClientID: "m2m", ClientSecret: "s3cret",
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	const callers = 3
	results := make([]<-chan userResult, 0, callers)
	results = append(results, startUser(context.Background(), l))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("token request did not start")
	}
	for i := 1; i < callers; i++ {
		results = append(results, startUser(context.Background(), l))
	}
	// Every caller has joined the failing refresh before it ends.
	waitWaiters(t, l, callers)
	unblock()
	for i, ch := range results {
		r := receive(t, ch, "caller")
		if r.err == nil || errors.Is(r.err, directory.ErrUserNotFound) || !strings.Contains(r.err.Error(), "503") {
			t.Fatalf("caller %d error = %v, want the shared 503 token error", i, r.err)
		}
	}
	if got := tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1 for the failed burst", got)
	}

	// A lookup that starts after the failure retries and caches the token.
	if r := receive(t, startUser(context.Background(), l), "retry"); r.err != nil || r.user.Name != "Ann" {
		t.Fatalf("retry = %+v, want the user", r)
	}
	if got := tokens.Load(); got != 2 {
		t.Fatalf("token requests after retry = %d, want 2", got)
	}
	if r := receive(t, startUser(context.Background(), l), "cached"); r.err != nil || r.user.Name != "Ann" {
		t.Fatalf("cached lookup = %+v, want the user", r)
	}
	if got := tokens.Load(); got != 2 {
		t.Fatalf("token requests after cached lookup = %d, want 2 (the token must be reused)", got)
	}
}

func TestLogtoCanceledWaiterDoesNotAffectSharedFailure(t *testing.T) {
	var tokens atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	defer unblock()
	l := directory.NewLogto(directory.LogtoConfig{
		Endpoint: srv.URL, ClientID: "m2m", ClientSecret: "s3cret",
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	leader := startUser(context.Background(), l)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("token request did not start")
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiter := startUser(waiterCtx, l)
	other := startUser(context.Background(), l)
	waitWaiters(t, l, 3)
	cancelWaiter()
	if r := receive(t, waiter, "canceled waiter"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", r.err)
	}
	unblock()
	for name, ch := range map[string]<-chan userResult{"leader": leader, "other": other} {
		if r := receive(t, ch, name); r.err == nil || !strings.Contains(r.err.Error(), "503") {
			t.Fatalf("%s error = %v, want the shared 503 token error", name, r.err)
		}
	}
	if got := tokens.Load(); got != 1 {
		t.Fatalf("token requests = %d, want 1", got)
	}
}
