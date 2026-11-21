// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand/internal/credstore"
	ownsand "github.com/delinoio/mono/go/ownsand"
)

// gatedRefreshToken is the refresh token whose refresh response the issuer
// holds until the test releases it.
const gatedRefreshToken = "gated-refresh"

// gatedLogto is a mock Logto issuer that holds the refresh response of
// gatedRefreshToken, so a test can run other commands while a refresh is in
// flight. Other refresh tokens (the one of a login) answer at once.
type gatedLogto struct {
	started chan struct{}
	release chan struct{}
}

func newGatedLogto() *gatedLogto {
	return &gatedLogto{started: make(chan struct{}, 4), release: make(chan struct{})}
}

func (g *gatedLogto) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oidc/device/auth":
			fmt.Fprint(w, `{"device_code":"dev-secret","user_code":"ABCD-EFGH","verification_uri":"https://login.example/device","expires_in":60,"interval":0}`)
		case r.URL.Path == "/oidc/token" && r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code":
			fmt.Fprint(w, `{"access_token":"opaque","refresh_token":"login-refresh-1","expires_in":3600}`)
		case r.URL.Path == "/oidc/token" && r.Form.Get("grant_type") == "refresh_token":
			if r.Form.Get("refresh_token") == gatedRefreshToken {
				g.started <- struct{}{}
				select {
				case <-g.release:
				case <-time.After(15 * time.Second):
					t.Error("the gated refresh was never released")
				}
				fmt.Fprint(w, `{"access_token":"gated-jwt","refresh_token":"gated-refresh-2","expires_in":3600}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"login-jwt","refresh_token":"login-refresh-2","expires_in":3600}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// refreshFixture is one credential store with an expired access token of an
// older session, shared by the app instances of a test.
type refreshFixture struct {
	r  *runner
	g  *gatedLogto
	st credstore.Store
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	g := newGatedLogto()
	ls := httptest.NewServer(g.handler(t))
	t.Cleanup(ls.Close)
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"
	st := credstore.Store{Dir: r.env[EnvConfigDir]}
	if err := st.Save(&credstore.Credentials{
		SessionID:         "old-session",
		LogtoEndpoint:     ls.URL,
		AppID:             "app",
		Resource:          DefaultLogtoResource,
		RefreshToken:      gatedRefreshToken,
		AccessToken:       "expired",
		AccessTokenExpiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return &refreshFixture{r: r, g: g, st: st}
}

// startGatedCommand starts `org get` in its own app instance and returns once
// its refresh request reached the issuer. wait releases nothing: it returns
// the command result after the test released the gate.
func (f *refreshFixture) startGatedCommand(t *testing.T) (wait func() result) {
	t.Helper()
	done := make(chan result, 1)
	go func() { done <- f.r.run("org", "get") }()
	select {
	case <-f.g.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh request never reached the issuer")
	}
	return func() result {
		select {
		case res := <-done:
			return res
		case <-time.After(15 * time.Second):
			t.Fatal("the command did not finish")
			return result{}
		}
	}
}

func TestLogoutDuringRefreshKeepsCredentialsDeleted(t *testing.T) {
	f := newRefreshFixture(t)
	wait := f.startGatedCommand(t)
	if res := f.r.run("logout"); res.code != 0 {
		t.Fatalf("logout: %+v", res)
	}
	close(f.g.release)
	res := wait()
	if res.code != ExitError || !strings.Contains(res.stderr, "signed out") || !strings.Contains(res.stderr, "ownsand login") {
		t.Fatalf("want a signed-out error, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.st.Dir, credstore.FileName)); !os.IsNotExist(err) {
		t.Fatalf("the late refresh recreated the credential file: %v", err)
	}
	for _, secret := range []string{"gated-jwt", "gated-refresh"} {
		if strings.Contains(res.stdout+res.stderr, secret) {
			t.Fatalf("output contains a token: %q", res.stdout+res.stderr)
		}
	}
	// The API was never called with the discarded token.
	if len(f.r.f.callNames()) != 0 {
		t.Fatalf("API called: %v", f.r.f.callNames())
	}
}

func TestLoginDuringRefreshKeepsNewerSession(t *testing.T) {
	f := newRefreshFixture(t)
	wait := f.startGatedCommand(t)
	if res := f.r.run("login"); res.code != 0 {
		t.Fatalf("login: %+v", res)
	}
	close(f.g.release)
	res := wait()
	if res.code != ExitError || !strings.Contains(res.stderr, "replaced") {
		t.Fatalf("want a replaced-credentials error, got %+v", res)
	}
	got, err := f.st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID == "old-session" || got.SessionID == "" || got.RefreshToken != "login-refresh-2" {
		t.Fatalf("the older refresh overwrote the newer login: session %q refresh %q", got.SessionID, got.RefreshToken)
	}
	if len(f.r.f.callNames()) != 0 {
		t.Fatalf("API called: %v", f.r.f.callNames())
	}
}

func TestRefreshWithoutConcurrentChangeStoresRotation(t *testing.T) {
	f := newRefreshFixture(t)
	close(f.g.release)
	res := f.r.run("org", "get")
	if res.code != 0 {
		t.Fatalf("%+v", res)
	}
	if got := f.r.f.lastAuth(); got != "Bearer gated-jwt" {
		t.Fatalf("Authorization %q", got)
	}
	got, err := f.st.Load()
	if err != nil || got.RefreshToken != "gated-refresh-2" || got.SessionID != "old-session" {
		t.Fatalf("rotation not stored: %+v %v", got, err)
	}
}

func TestRefreshOfCredentialsWithoutSessionIDStoresRotation(t *testing.T) {
	f := newRefreshFixture(t)
	close(f.g.release)
	// A file written before session identifiers existed.
	cur, err := f.st.Load()
	if err != nil {
		t.Fatal(err)
	}
	cur.SessionID = ""
	if err := f.st.Save(cur); err != nil {
		t.Fatal(err)
	}
	if res := f.r.run("org", "get"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	got, err := f.st.Load()
	if err != nil || got.RefreshToken != "gated-refresh-2" {
		t.Fatalf("rotation not stored: %+v %v", got, err)
	}
}
