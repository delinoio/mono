// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORS(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandler(logger,
		WithCORSOrigins([]string{"https://accounts.delino.io"}),
		WithMounts(Mount{Path: "/x/", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})}),
	)
	do := func(method, origin string, preflight bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/x/y", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", "POST")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodOptions, "https://accounts.delino.io", true)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://accounts.delino.io" ||
		rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Fatalf("allowed preflight: %d %v", rec.Code, rec.Header())
	}
	rec = do(http.MethodOptions, "https://evil.example", true)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other preflight: %d %v", rec.Code, rec.Header())
	}
	rec = do(http.MethodPost, "https://accounts.delino.io", false)
	if rec.Code != http.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "https://accounts.delino.io" {
		t.Fatalf("allowed request: %d %v", rec.Code, rec.Header())
	}
	rec = do(http.MethodPost, "https://evil.example", false)
	if rec.Code != http.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other request: %d %v", rec.Code, rec.Header())
	}
	rec = do(http.MethodPost, "", false)
	if rec.Code != http.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("request without origin: %d %v", rec.Code, rec.Header())
	}
}

// TestCORSMatchesBrowserSerializedOrigin checks that the middleware, which
// compares the Origin header by exact string match, accepts the form that a
// browser sends for an origin that config.Load canonicalized (lower-case host,
// no default port) and keeps other ports distinct.
func TestCORSMatchesBrowserSerializedOrigin(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandler(logger, WithCORSOrigins([]string{"https://accounts.delino.io", "http://accounts.delino.localhost:8080", "http://127.0.0.1:3000"}))
	for origin, want := range map[string]int{
		"https://accounts.delino.io":            http.StatusNoContent,
		"http://accounts.delino.localhost:8080": http.StatusNoContent,
		"https://accounts.delino.io:8443":       http.StatusForbidden,
		"http://accounts.delino.localhost":      http.StatusForbidden,
		"http://accounts.delino.io":             http.StatusForbidden,
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
