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
	h := NewHandler(Options{CORSOrigins: []string{"https://tide.delino.io"}, Mounts: []Mount{{Path: "/x/", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}}})
	pre := httptest.NewRequest(http.MethodOptions, "/x/y", nil)
	pre.Header.Set("Origin", "https://tide.delino.io")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://tide.delino.io" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Connect-Protocol-Version") {
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
