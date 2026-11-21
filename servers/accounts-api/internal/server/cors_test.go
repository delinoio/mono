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
