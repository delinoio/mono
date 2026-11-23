// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	h := NewHandler(Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestCORS(t *testing.T) {
	h := NewHandler(Options{CORSOrigins: []string{"https://ownsand.delino.io"}, Mounts: []Mount{{Path: "/x/", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}}})
	pre := httptest.NewRequest(http.MethodOptions, "/x/y", nil)
	pre.Header.Set("Origin", "https://ownsand.delino.io")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://ownsand.delino.io" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Ownsand-Relay-Grant-Id") {
		t.Fatalf("preflight = %d %v", rec.Code, rec.Header())
	}
	evil := httptest.NewRequest(http.MethodOptions, "/x/y", nil)
	evil.Header.Set("Origin", "https://evil.example")
	evil.Header.Set("Access-Control-Request-Method", "POST")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, evil)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign preflight = %d %v", rec.Code, rec.Header())
	}
}

// TestCORSMatchesBrowserSerializedOrigin checks that the middleware, which
// compares the Origin header by exact string match, accepts the form that a
// browser sends for an origin that config.Load canonicalized (lower-case host,
// no default port), keeps other ports distinct and sends Vary: Origin.
func TestCORSMatchesBrowserSerializedOrigin(t *testing.T) {
	h := NewHandler(Options{CORSOrigins: []string{"https://ownsand.delino.io", "http://ownsand.delino.localhost:8080", "http://127.0.0.1:3000"}})
	for origin, want := range map[string]int{
		"https://ownsand.delino.io":            http.StatusNoContent,
		"http://ownsand.delino.localhost:8080": http.StatusNoContent,
		"https://ownsand.delino.io:8443":       http.StatusForbidden,
		"http://ownsand.delino.localhost":      http.StatusForbidden,
		"http://ownsand.delino.io":             http.StatusForbidden,
		// config.Load stores http://127.1:3000 as this origin, which a browser sends.
		"http://127.0.0.1:3000": http.StatusNoContent,
		"http://127.0.0.1:3001": http.StatusForbidden,
		"http://127.0.0.2:3000": http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodOptions, "/x/y", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("preflight from %s: status = %d, want %d", origin, rec.Code, want)
		}
		if want == http.StatusNoContent && (rec.Header().Get("Access-Control-Allow-Origin") != origin || rec.Header().Get("Vary") != "Origin") {
			t.Errorf("preflight from %s: headers = %v", origin, rec.Header())
		}
	}
}
