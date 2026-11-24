// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/tide-api/internal/config"
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

func TestCORSMatchesBrowserSerializedOrigin(t *testing.T) {
	env := map[string]string{
		config.EnvDatabaseURL: "postgres://tide@127.0.0.1/tide",
		config.EnvLogtoIssuer: "https://tenant.logto.app/oidc",
		config.EnvSecretKeys:  "k1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		config.EnvCORSOrigins: "HTTPS://Tide.Delino.io:443",
	}
	cfg, err := config.Load(func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Options{CORSOrigins: cfg.CORSOrigins})
	for _, tc := range []struct {
		origin string
		status int
		allow  string
		vary   string
	}{
		{"https://tide.delino.io", http.StatusNoContent, "https://tide.delino.io", "Origin"},
		{"https://tide.delino.io:8443", http.StatusForbidden, "", ""},
		{"http://tide.delino.io", http.StatusForbidden, "", ""},
	} {
		req := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status || rec.Header().Get("Access-Control-Allow-Origin") != tc.allow || rec.Header().Get("Vary") != tc.vary {
			t.Errorf("preflight from %s = %d %v, want %d with allow origin %q and Vary %q", tc.origin, rec.Code, rec.Header(), tc.status, tc.allow, tc.vary)
		}
	}
}
