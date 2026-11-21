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
	w.Header().Set("Content-Type", "application/json")
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":"secret-description"}`, s.errorStr)
		return
	}
	_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d,"scope":"accounts:report-usage"}`, n, s.expires)
}

func newSource(t *testing.T, ts *tokenServer, c *clock) *TokenSource {
	srv := httptest.NewServer(ts)
	t.Cleanup(srv.Close)
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
