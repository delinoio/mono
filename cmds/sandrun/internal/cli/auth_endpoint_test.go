// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

type loginEndpointTransport struct {
	requests []string
}

func (tr *loginEndpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Body.Close()
	tr.requests = append(tr.requests, req.Method+" "+req.URL.EscapedPath())
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":"stop_after_device_request","error_description":"synthetic-description-marker"}`)),
	}, nil
}

func TestLoginValidatesLogtoEndpoint(t *testing.T) {
	const tenantEndpoint = "http://auth.example:8080/tenant"
	const queryOrFragmentError = "logto: the tenant endpoint must not contain a query or fragment"
	for _, tc := range []struct {
		name         string
		endpoint     string
		wantRequests []string
		wantError    string
	}{
		{name: "query", endpoint: tenantEndpoint + "?mode=test", wantError: queryOrFragmentError},
		{name: "fragment", endpoint: tenantEndpoint + "#login", wantError: queryOrFragmentError},
		{name: "empty query", endpoint: tenantEndpoint + "?", wantError: queryOrFragmentError},
		{name: "empty fragment", endpoint: tenantEndpoint + "#", wantError: queryOrFragmentError},
		{
			name:      "port without hostname",
			endpoint:  "https://:443",
			wantError: "logto: the tenant endpoint must be an http or https URL",
		},
		{
			name:         "HTTPS with explicit port",
			endpoint:     "https://auth.example:8443",
			wantRequests: []string{"POST /oidc/device/auth"},
			wantError:    "logto: http_400",
		},
		{
			name:         "path prefix with trailing slash",
			endpoint:     tenantEndpoint + "/",
			wantRequests: []string{"POST /tenant/oidc/device/auth"},
			wantError:    "logto: http_400",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{EnvLogtoEndpoint: tc.endpoint, EnvLogtoAppID: "app", EnvNoColor: "1"}
			tr := &loginEndpointTransport{}
			var stderr bytes.Buffer
			code := Run(context.Background(), Env{
				Args:       []string{"login"},
				Stdout:     io.Discard,
				Stderr:     &stderr,
				LookupEnv:  func(key string) (string, bool) { value, ok := values[key]; return value, ok },
				HTTPClient: &http.Client{Transport: tr},
			})
			if got := stderr.String(); code != ExitError || got != "error: "+tc.wantError+"\n" {
				t.Errorf("login exit = %d, stderr = %q, want exit %d with %q", code, got, ExitError, tc.wantError)
			}
			if !slices.Equal(tr.requests, tc.wantRequests) {
				t.Errorf("requests = %v, want %v", tr.requests, tc.wantRequests)
			}
		})
	}
}
