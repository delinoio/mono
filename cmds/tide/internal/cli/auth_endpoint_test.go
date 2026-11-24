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
		Body:       io.NopCloser(strings.NewReader(`{"error":"stop_after_device_request"}`)),
	}, nil
}

func TestLoginValidatesLogtoEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		suffix       string
		wantRequests []string
		wantError    string
	}{
		{name: "query", suffix: "?mode=test"},
		{name: "fragment", suffix: "#login"},
		{name: "empty query", suffix: "?"},
		{name: "empty fragment", suffix: "#"},
		{
			name:         "path prefix with trailing slash",
			suffix:       "/",
			wantRequests: []string{"POST /tenant/oidc/device/auth"},
			wantError:    "logto: stop_after_device_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "http://auth.example:8080/tenant" + tc.suffix
			values := map[string]string{EnvLogtoEndpoint: endpoint, EnvLogtoAppID: "app", EnvNoColor: "1"}
			tr := &loginEndpointTransport{}
			var stderr bytes.Buffer
			code := Run(context.Background(), Env{
				Args:       []string{"login"},
				Stdout:     io.Discard,
				Stderr:     &stderr,
				LookupEnv:  func(key string) (string, bool) { value, ok := values[key]; return value, ok },
				HTTPClient: &http.Client{Transport: tr},
			})
			wantError := tc.wantError
			if wantError == "" {
				wantError = "logto: the tenant endpoint must not contain a query or fragment"
			}
			if got := stderr.String(); code != ExitError || !strings.Contains(got, wantError) || strings.Contains(got, endpoint) {
				t.Errorf("login exit = %d, stderr = %q, want exit %d with %q", code, got, ExitError, wantError)
			}
			if !slices.Equal(tr.requests, tc.wantRequests) {
				t.Errorf("requests = %v, want %v", tr.requests, tc.wantRequests)
			}
		})
	}
}
