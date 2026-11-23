// SPDX-License-Identifier: FSL-1.1-ALv2

package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// tokenServer is a fake Logto token endpoint.
type tokenServer struct {
	t        *testing.T
	calls    atomic.Int64
	expires  int64
	status   int
	errorStr string
	// When body is set, a response writes it instead of the default valid
	// token response or OAuth error body.
	body string
	// When block is set, a request announces itself on entered and then
	// waits until block is closed.
	block       chan struct{}
	entered     chan struct{}
	releaseOnce sync.Once
}

// release lets the held requests continue. It is safe to call twice.
func (s *tokenServer) release() {
	if s.block != nil {
		s.releaseOnce.Do(func() { close(s.block) })
	}
}

func (s *tokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		s.t.Errorf("parse form: %v", err)
	}
	id, secret, ok := r.BasicAuth()
	if r.Method != http.MethodPost || !ok || id != "client%2Fid" || secret != "s%26cret" ||
		r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("resource") != Resource || r.PostForm.Get("scope") != ScopeReportUsage {
		s.t.Errorf("token request: %s %v %q %q %v", r.Method, ok, id, secret, r.PostForm)
	}
	if s.block != nil {
		s.entered <- struct{}{}
		<-s.block
	}
	w.Header().Set("Content-Type", "application/json")
	if s.status != 0 {
		w.WriteHeader(s.status)
		if s.body != "" {
			_, _ = w.Write([]byte(s.body))
			return
		}
		_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":"secret-description"}`, s.errorStr)
		return
	}
	if s.body != "" {
		_, _ = w.Write([]byte(s.body))
		return
	}
	_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d,"scope":"accounts:report-usage"}`, n, s.expires)
}

func newSource(t *testing.T, ts *tokenServer, c *clock) *TokenSource {
	srv := httptest.NewServer(ts)
	t.Cleanup(srv.Close)
	// Runs before srv.Close, so a failed test never hangs on a held request.
	t.Cleanup(ts.release)
	return NewTokenSource(TokenConfig{Endpoint: srv.URL + "/oidc/token", ClientID: "client/id", ClientSecret: "s&cret", HTTPClient: srv.Client(), Now: c.Now})
}

func TestTokenSourceCachesUntilShortlyBeforeExpiry(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, expires: 3600}
	src := newSource(t, ts, c)
	ctx := context.Background()
	for range 3 {
		tok, err := src.Token(ctx)
		if err != nil || tok != "token-1" {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	// Still cached just before the refresh point (expiry minus one minute).
	c.Advance(59*time.Minute - time.Second)
	if tok, _ := src.Token(ctx); tok != "token-1" {
		t.Fatalf("token before the refresh point = %q", tok)
	}
	c.Advance(time.Second)
	if tok, _ := src.Token(ctx); tok != "token-2" {
		t.Fatalf("token at the refresh point = %q", tok)
	}
	src.Invalidate()
	if tok, _ := src.Token(ctx); tok != "token-3" || ts.calls.Load() != 3 {
		t.Fatalf("token after invalidate = %q (calls %d)", tok, ts.calls.Load())
	}
}

type tokenResult struct {
	token string
	err   error
}

func tokenAsync(src *TokenSource, ctx context.Context) <-chan tokenResult {
	ch := make(chan tokenResult, 1)
	go func() {
		token, err := src.Token(ctx)
		ch <- tokenResult{token, err}
	}()
	return ch
}

func receive(t *testing.T, ch <-chan tokenResult, what string) tokenResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return tokenResult{}
	}
}

// waitForFlightWaiters blocks until n callers wait on the token request in
// progress, so a test acts only after a second caller joined the flight.
func waitForFlightWaiters(t *testing.T, src *TokenSource, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for src.flightWaiters() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d callers did not wait on the token request (have %d)", n, src.flightWaiters())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTokenSourceWaiterCancelDoesNotEndTheSharedRefresh(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, expires: 3600, block: make(chan struct{}), entered: make(chan struct{}, 4)}
	src := newSource(t, ts, c)

	first := tokenAsync(src, context.Background())
	<-ts.entered // the first caller's request is held open
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiter := tokenAsync(src, waiterCtx)
	waitForFlightWaiters(t, src, 2) // the waiter joined the request before it is cancelled
	cancel()
	// The cancelled waiter returns while the request is still held open.
	if r := receive(t, waiter, "the cancelled waiter"); !errors.Is(r.err, context.Canceled) || r.token != "" {
		t.Fatalf("cancelled waiter = %+v", r)
	}
	select {
	case r := <-first:
		t.Fatalf("the first caller returned early: %+v", r)
	default:
	}
	// Invalidate does not wait for the request either.
	invalidated := make(chan struct{})
	go func() { src.Invalidate(); close(invalidated) }()
	select {
	case <-invalidated:
	case <-time.After(5 * time.Second):
		t.Fatal("Invalidate blocked behind the token request")
	}
	ts.release()
	// The first caller still gets the token of the request it started.
	if r := receive(t, first, "the first caller"); r.err != nil || r.token != "token-1" {
		t.Fatalf("first caller = %+v", r)
	}
}

func TestTokenSourceCallersShareOneRefreshAndTheCache(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, expires: 3600, block: make(chan struct{}), entered: make(chan struct{}, 4)}
	src := newSource(t, ts, c)

	first := tokenAsync(src, context.Background())
	<-ts.entered
	second := tokenAsync(src, context.Background())
	waitForFlightWaiters(t, src, 2) // the second caller joined the request before it is released
	ts.release()
	for name, ch := range map[string]<-chan tokenResult{"first": first, "second": second} {
		if r := receive(t, ch, name); r.err != nil || r.token != "token-1" {
			t.Fatalf("%s caller = %+v", name, r)
		}
	}
	// A third caller reuses the cached token.
	if tok, err := src.Token(context.Background()); err != nil || tok != "token-1" || ts.calls.Load() != 1 {
		t.Fatalf("third caller = %q, %v (calls %d)", tok, err, ts.calls.Load())
	}
}

func TestTokenSourceCancelledCallerDoesNotStartARequest(t *testing.T) {
	c := &clock{now: time.Now()}
	ts := &tokenServer{t: t, expires: 3600}
	src := newSource(t, ts, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Token(ctx); !errors.Is(err, context.Canceled) || ts.calls.Load() != 0 {
		t.Fatalf("cancelled caller: %v (calls %d)", err, ts.calls.Load())
	}
}

func TestTokenSourceInvalidateDuringRefreshStartsANewRefresh(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, expires: 3600, block: make(chan struct{}), entered: make(chan struct{}, 4)}
	src := newSource(t, ts, c)

	stale := tokenAsync(src, context.Background())
	<-ts.entered
	src.Invalidate()
	fresh := tokenAsync(src, context.Background())
	<-ts.entered // the invalidated source started a second request
	ts.release()
	receive(t, stale, "the caller of the invalidated request")
	r := receive(t, fresh, "the caller of the new request")
	if r.err != nil || r.token != "token-2" {
		t.Fatalf("new request = %+v", r)
	}
	// Only the new request is cached.
	if tok, _ := src.Token(context.Background()); tok != "token-2" || ts.calls.Load() != 2 {
		t.Fatalf("cached token = %q (calls %d)", tok, ts.calls.Load())
	}
}

func TestTokenSourceShortLifetime(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, expires: 60}
	src := newSource(t, ts, c)
	ctx := context.Background()
	if tok, _ := src.Token(ctx); tok != "token-1" {
		t.Fatal(tok)
	}
	c.Advance(29 * time.Second)
	if tok, _ := src.Token(ctx); tok != "token-1" {
		t.Fatalf("within half the lifetime: %q", tok)
	}
	c.Advance(time.Second)
	if tok, _ := src.Token(ctx); tok != "token-2" {
		t.Fatalf("after half the lifetime: %q", tok)
	}
}

func TestTokenSourceError(t *testing.T) {
	c := &clock{now: time.Now()}
	ts := &tokenServer{t: t, status: http.StatusUnauthorized, errorStr: "invalid_client"}
	src := newSource(t, ts, c)
	_, err := src.Token(context.Background())
	var te *TokenError
	if !errors.As(err, &te) || te.Status != http.StatusUnauthorized || te.Code != "invalid_client" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "s&cret") || strings.Contains(err.Error(), "secret-description") {
		t.Fatalf("the error leaks the response or the secret: %v", err)
	}
	ts.errorStr = "<script>"
	if _, err := src.Token(context.Background()); !errors.As(err, &te) || te.Code != "" {
		t.Fatalf("unexpected error code kept: %v", err)
	}
}

// usageHandler is a fake UsageService that records the Authorization header
// and fails the first call with unauthenticated.
type usageHandler struct {
	accountsv1connect.UnimplementedUsageServiceHandler
	mu      sync.Mutex
	headers []string
}

func (h *usageHandler) ReportUsage(_ context.Context, req *connect.Request[accountsv1.ReportUsageRequest]) (*connect.Response[accountsv1.ReportUsageResponse], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.headers = append(h.headers, req.Header().Get("Authorization"))
	if len(h.headers) == 1 {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("expired token"))
	}
	return connect.NewResponse(&accountsv1.ReportUsageResponse{}), nil
}

func TestUsageClientSendsTokenAndDropsRejectedToken(t *testing.T) {
	c := &clock{now: time.Now()}
	ts := &tokenServer{t: t, expires: 3600}
	src := newSource(t, ts, c)
	h := &usageHandler{}
	mux := http.NewServeMux()
	mux.Handle(accountsv1connect.NewUsageServiceHandler(h))
	api := httptest.NewServer(mux)
	defer api.Close()
	client := NewUsageClient(api.Client(), api.URL+"/", src)
	ctx := context.Background()
	if _, err := client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("first call: %v", err)
	}
	if _, err := client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{})); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(h.headers) != 2 || h.headers[0] != "Bearer token-1" || h.headers[1] != "Bearer token-2" {
		t.Fatalf("headers = %v", h.headers)
	}
	// A failing token endpoint fails the call with unavailable.
	src.Invalidate()
	ts.status = http.StatusServiceUnavailable
	if _, err := client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("token failure: %v", err)
	}
}

func TestTokenSourceRejectsMalformedSuccessfulResponse(t *testing.T) {
	bodies := map[string]string{
		"expires_in as a string":    `{"access_token":"leaked-token","token_type":"Bearer","expires_in":"3600"}`,
		"token_type as a number":    `{"access_token":"leaked-token","token_type":7,"expires_in":3600}`,
		"access_token as an object": `{"access_token":{"v":"leaked-token"},"token_type":"Bearer","expires_in":3600}`,
		"truncated document":        `{"access_token":"leaked-token","token_type":"Bearer"`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
			ts := &tokenServer{t: t, expires: 3600, body: body}
			src := newSource(t, ts, c)
			ctx := context.Background()
			tok, err := src.Token(ctx)
			if err == nil || tok != "" {
				t.Fatalf("token = %q, %v", tok, err)
			}
			var te *TokenError
			if errors.As(err, &te) {
				t.Fatalf("a malformed 200 response is no OAuth error: %v", err)
			}
			for _, leak := range []string{"leaked-token", "s&cret", "3600", "Bearer"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error leaks %q: %v", leak, err)
				}
			}
			// Nothing was cached: the next call asks the endpoint again,
			// and a valid response then succeeds.
			if _, err := src.Token(ctx); err == nil || ts.calls.Load() != 2 {
				t.Fatalf("second call: %v (calls %d)", err, ts.calls.Load())
			}
			ts.body = ""
			if tok, err := src.Token(ctx); err != nil || tok != "token-3" {
				t.Fatalf("token after a valid response = %q, %v", tok, err)
			}
			if tok, _ := src.Token(ctx); tok != "token-3" || ts.calls.Load() != 3 {
				t.Fatalf("valid token is cached: %q (calls %d)", tok, ts.calls.Load())
			}
		})
	}
}

func TestTokenSourceAcceptsValidDocumentWithoutTokenTypeOrExpiry(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	ts := &tokenServer{t: t, body: `{"access_token":"plain-token"}`}
	src := newSource(t, ts, c)
	ctx := context.Background()
	if tok, err := src.Token(ctx); err != nil || tok != "plain-token" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	// A missing expiry caches the token for 2*refreshMargin, refreshed
	// at half of that (one minute).
	c.Advance(refreshMargin - time.Second)
	if _, err := src.Token(ctx); err != nil || ts.calls.Load() != 1 {
		t.Fatalf("within the brief lifetime: %v (calls %d)", err, ts.calls.Load())
	}
	c.Advance(time.Second)
	if _, err := src.Token(ctx); err != nil || ts.calls.Load() != 2 {
		t.Fatalf("after the brief lifetime: %v (calls %d)", err, ts.calls.Load())
	}
}

func TestTokenSourceIgnoresDecodeErrorOfAnErrorResponse(t *testing.T) {
	c := &clock{now: time.Now()}
	ts := &tokenServer{t: t, status: http.StatusBadRequest, errorStr: "invalid_scope"}
	src := newSource(t, ts, c)
	var te *TokenError
	if _, err := src.Token(context.Background()); !errors.As(err, &te) || te.Status != http.StatusBadRequest || te.Code != "invalid_scope" {
		t.Fatalf("error = %v", err)
	}
	// A type mismatch next to a valid code still yields the code, and an
	// undecodable body yields none, without a decode diagnostic.
	ts.body = `{"error":"invalid_client","expires_in":"x"}`
	if _, err := src.Token(context.Background()); !errors.As(err, &te) || te.Code != "invalid_client" {
		t.Fatalf("error with a type mismatch = %v", err)
	}
	ts.body = `not json`
	if _, err := src.Token(context.Background()); !errors.As(err, &te) || te.Status != http.StatusBadRequest || te.Code != "" {
		t.Fatalf("error with an undecodable body = %v", err)
	}
}
