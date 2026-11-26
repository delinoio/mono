// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLogtoRedirectsStayOnTenantOrigin(t *testing.T) {
	for _, client := range []struct {
		name string
		http *http.Client
	}{
		{"default", nil},
		{"injected", &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}},
	} {
		t.Run(client.name, func(t *testing.T) {
			t.Run("device flow", func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if !strings.HasPrefix(r.URL.Path, "/same") {
						http.Redirect(w, r, "/same"+r.URL.Path, http.StatusTemporaryRedirect)
						return
					}
					if r.Method != http.MethodPost || r.FormValue("client_id") != "test-app" {
						t.Error("redirect lost the POST method or client ID")
					}
					switch r.URL.Path {
					case "/same/oidc/device/auth":
						io.WriteString(w, `{"device_code":"device","user_code":"TEST","verification_uri":"https://example.invalid"}`)
					case "/same/oidc/token":
						switch r.FormValue("grant_type") {
						case "urn:ietf:params:oauth:grant-type:device_code":
							if r.FormValue("device_code") != "device" {
								t.Error("redirect lost the device code")
							}
						case "refresh_token":
							if r.FormValue("refresh_token") != "refresh" {
								t.Error("redirect lost the refresh token")
							}
						default:
							t.Error("redirect lost the grant type")
						}
						io.WriteString(w, `{"access_token":"access","refresh_token":"refresh"}`)
					default:
						t.Errorf("unexpected path %q", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				cfg := Config{Endpoint: server.URL, AppID: "test-app", HTTPClient: client.http}
				ctx := context.Background()
				da, err := cfg.StartDeviceAuthorization(ctx)
				if err != nil {
					t.Fatal(err)
				}
				tok, err := cfg.PollToken(ctx, da, time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				refreshed, err := cfg.Refresh(ctx, tok.RefreshToken)
				if err != nil || refreshed.AccessToken != "access" {
					t.Fatalf("refresh did not return the access token: %v", err)
				}
				if got := requests.Load(); got != 6 {
					t.Fatalf("made %d requests, want 6", got)
				}
			})

			t.Run("blocked hop", func(t *testing.T) {
				var targetRequests atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					targetRequests.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer target.Close()
				issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next := "/same"
					if r.URL.Path == next {
						next = target.URL + "/private?data=placeholder"
					}
					http.Redirect(w, r, next, http.StatusTemporaryRedirect)
				}))
				defer issuer.Close()
				cfg := Config{Endpoint: issuer.URL, AppID: "test-app", HTTPClient: client.http}
				tok, err := cfg.Refresh(context.Background(), "placeholder")
				if got := targetRequests.Load(); got != 0 {
					t.Fatalf("different origin received %d requests, want 0", got)
				}
				const diagnostic = "logto: redirect leaves the configured tenant origin"
				if tok != nil || err == nil || err.Error() != diagnostic {
					t.Fatalf("got token %t and error %v, want no token and %q", tok != nil, err, diagnostic)
				}
			})

			t.Run("limit", func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
				}))
				defer server.Close()
				cfg := Config{Endpoint: server.URL, AppID: "test-app", HTTPClient: client.http}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := cfg.Refresh(ctx, "placeholder")
				if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
					t.Fatalf("got %v, want the redirect limit error", err)
				}
				if got := requests.Load(); got != 10 {
					t.Fatalf("made %d requests, want 10", got)
				}
			})

			t.Run("cancellation", func(t *testing.T) {
				atDestination := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/same" {
						http.Redirect(w, r, "/same", http.StatusTemporaryRedirect)
						return
					}
					io.Copy(io.Discard, r.Body)
					close(atDestination)
					<-r.Context().Done()
				}))
				defer server.Close()
				cfg := Config{Endpoint: server.URL, AppID: "test-app", HTTPClient: client.http}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result := make(chan error, 1)
				go func() {
					_, err := cfg.Refresh(ctx, "placeholder")
					result <- err
				}()
				select {
				case <-atDestination:
					cancel()
				case err := <-result:
					t.Fatalf("request ended before the second hop: %v", err)
				}
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want context canceled", err)
				}
			})
		})
	}
}

func TestLogtoRedirectKeepsCallerPolicy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/same", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	refused := errors.New("caller refused redirect")
	policyCalls := 0
	caller := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		policyCalls++
		return refused
	}}
	cfg := Config{Endpoint: server.URL, AppID: "test-app", HTTPClient: caller}
	_, err := cfg.Refresh(context.Background(), "placeholder")
	if !errors.Is(err, refused) || requests.Load() != 1 || policyCalls != 1 {
		t.Fatalf("got error %v, requests %d, policy calls %d, want caller refusal after one request",
			err, requests.Load(), policyCalls)
	}
	if err := caller.CheckRedirect(nil, nil); err != refused {
		t.Fatal("the caller's redirect policy changed")
	}
}

func TestLogtoRequestOrigin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant string
		target string
		same   bool
	}{
		{"https default port and case", "https://ISSUER.example", "https://issuer.example:443/poll", true},
		{"http default port", "http://issuer.example", "http://issuer.example:80/poll", true},
		{"leading port zeros", "http://issuer.example:08080", "http://issuer.example:8080/poll", true},
		{"idna hostname", "https://bücher.example", "https://xn--bcher-kva.example/poll", true},
		{"equivalent IPv6", "http://[2001:DB8::1]", "http://[2001:db8:0:0::1]:80/poll", true},
		{"different IPv6 zone", "http://[fe80::1%25if1]", "http://[fe80::1%25IF1]/poll", false},
		{"different scheme", "https://issuer.example", "http://issuer.example:443/poll", false},
		{"different hostname", "https://issuer.example", "https://other.issuer.example/poll", false},
		{"different port", "https://issuer.example", "https://issuer.example:444/poll", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant, err := url.Parse(tc.tenant)
			if err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if same := requestOrigin(tenant) == requestOrigin(target); same != tc.same {
				t.Fatalf("same origin = %v, want %v", same, tc.same)
			}
		})
	}
}

func TestLogtoRedirectChecksCallerRewrite(t *testing.T) {
	requests := 0
	caller := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"/same"}},
				Body:       http.NoBody,
			}, nil
		}),
		CheckRedirect: func(next *http.Request, _ []*http.Request) error {
			next.URL.Host = "other.example"
			return nil
		},
	}
	cfg := Config{Endpoint: "https://issuer.example", AppID: "test-app", HTTPClient: caller}
	_, err := cfg.Refresh(context.Background(), "placeholder")
	if err != errRedirectOrigin || requests != 1 {
		t.Fatalf("got error %v after %d requests, want origin rejection after one request", err, requests)
	}
}
