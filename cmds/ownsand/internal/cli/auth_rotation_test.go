// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand/internal/credstore"
)

// rotatingLogto is a mock Logto issuer that rotates refresh tokens like the
// real one: a refresh token works once, and a spent one is answered with
// invalid_grant. hook runs after the request arrived and before the issuer
// decides, so a test can hold a request while other processes act.
type rotatingLogto struct {
	mu        sync.Mutex
	valid     map[string]bool
	calls     []string
	seq       int
	expiresIn int
	hook      func(refreshToken string)
}

func newRotatingLogto(valid ...string) *rotatingLogto {
	l := &rotatingLogto{valid: map[string]bool{}, expiresIn: 3600}
	for _, v := range valid {
		l.valid[v] = true
	}
	return l
}

// setValid replaces the set of refresh tokens that the issuer accepts.
func (l *rotatingLogto) setValid(tokens ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.valid = map[string]bool{}
	for _, v := range tokens {
		l.valid[v] = true
	}
}

func (l *rotatingLogto) refreshCalls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

func (l *rotatingLogto) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/oidc/token" || r.Form.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rt := r.Form.Get("refresh_token")
		l.mu.Lock()
		l.calls = append(l.calls, rt)
		hook := l.hook
		l.mu.Unlock()
		if hook != nil {
			hook(rt)
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.valid[rt] {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"grant request is invalid"}`)
			return
		}
		delete(l.valid, rt)
		l.seq++
		l.valid[fmt.Sprintf("rt-%d", l.seq)] = true
		fmt.Fprintf(w, `{"access_token":"jwt-%d","refresh_token":"rt-%d","expires_in":%d}`, l.seq, l.seq, l.expiresIn)
	})
}

type rotationFixture struct {
	t   *testing.T
	l   *rotatingLogto
	url string
	st  credstore.Store
	env map[string]string
}

// newRotationFixture stores an expired access token of the session "s" with
// the refresh token "rt-0", which the issuer accepts.
func newRotationFixture(t *testing.T) *rotationFixture {
	t.Helper()
	l := newRotatingLogto("rt-0")
	ls := httptest.NewServer(l.handler())
	t.Cleanup(ls.Close)
	f := &rotationFixture{t: t, l: l, url: ls.URL, st: credstore.Store{Dir: t.TempDir()}}
	f.env = map[string]string{EnvConfigDir: f.st.Dir}
	f.save("s", "rt-0", "expired", time.Now().Add(-time.Hour))
	return f
}

func (f *rotationFixture) save(session, refresh, access string, expiry time.Time) {
	f.t.Helper()
	if err := f.st.Save(&credstore.Credentials{
		SessionID: session, LogtoEndpoint: f.url, AppID: "app", Resource: DefaultLogtoResource,
		RefreshToken: refresh, AccessToken: access, AccessTokenExpiry: expiry,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// source returns the token source of one process: it loads the stored
// credentials when it is created.
func (f *rotationFixture) source() func(context.Context) (string, error) {
	f.t.Helper()
	a := &app{
		env:    Env{LookupEnv: func(k string) (string, bool) { v, ok := f.env[k]; return v, ok }},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts, err := a.tokenSource()
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

func (f *rotationFixture) stored() *credstore.Credentials {
	f.t.Helper()
	c, err := f.st.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// A source that finds the access token another process refreshed uses it and
// sends no request: the refresh token it loaded is spent.
func TestTokenSourceAdoptsRefreshOfAnotherProcess(t *testing.T) {
	f := newRotationFixture(t)
	a, b := f.source(), f.source()
	if got, err := a(context.Background()); err != nil || got != "jwt-1" {
		t.Fatalf("first process: %q %v", got, err)
	}
	if got, err := b(context.Background()); err != nil || got != "jwt-1" {
		t.Fatalf("second process: %q %v", got, err)
	}
	if calls := f.l.refreshCalls(); !slices.Equal(calls, []string{"rt-0"}) {
		t.Fatalf("refresh requests %v, want one", calls)
	}
}

// A source whose cached refresh token is spent, and whose stored access token
// needs a refresh again, refreshes with the stored refresh token.
func TestTokenSourceRefreshesWithStoredRefreshToken(t *testing.T) {
	f := newRotationFixture(t)
	f.l.expiresIn = 1 // inside refreshMargin: every result needs a refresh again
	a, b := f.source(), f.source()
	if _, err := a(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.l.expiresIn = 3600
	if got, err := b(context.Background()); err != nil || got != "jwt-2" {
		t.Fatalf("second process: %q %v", got, err)
	}
	if calls := f.l.refreshCalls(); !slices.Equal(calls, []string{"rt-0", "rt-1"}) {
		t.Fatalf("refresh requests %v, want rt-0 then rt-1", calls)
	}
	if got := f.stored(); got.RefreshToken != "rt-2" || got.SessionID != "s" {
		t.Fatalf("stored %+v", got)
	}
}

// A refresh that finishes after another process stored a newer rotation of
// the same session must not replace it.
func TestStaleRefreshDoesNotOverwriteNewerRotation(t *testing.T) {
	f := newRotationFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.l.hook = func(string) {
		close(started)
		<-release
	}
	type out struct {
		tok string
		err error
	}
	done := make(chan out, 1)
	go func() {
		tok, err := f.source()(context.Background())
		done <- out{tok, err}
	}()
	<-started
	// Another process refreshed and stored its rotation meanwhile.
	f.save("s", "rt-newer", "newer-jwt", time.Now().Add(time.Hour))
	close(release)
	res := <-done
	if res.err != nil || res.tok != "newer-jwt" {
		t.Fatalf("got %q %v, want the newer stored token", res.tok, res.err)
	}
	got := f.stored()
	if got.RefreshToken != "rt-newer" || got.AccessToken != "newer-jwt" {
		t.Fatalf("the stale refresh overwrote the newer rotation: %+v", got)
	}
}

// A refresh token that another process spent while the request was in flight
// is rejected with invalid_grant; the source retries with the stored one.
func TestInvalidGrantRetriesWithRotatedStoredToken(t *testing.T) {
	f := newRotationFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.l.hook = func(rt string) {
		if rt != "rt-0" {
			return
		}
		close(started)
		<-release
	}
	type out struct {
		tok string
		err error
	}
	done := make(chan out, 1)
	go func() {
		tok, err := f.source()(context.Background())
		done <- out{tok, err}
	}()
	<-started
	// Another process rotated rt-0 to rt-other; its access token is expired.
	f.l.setValid("rt-other")
	f.save("s", "rt-other", "expired", time.Now().Add(-time.Hour))
	close(release)
	res := <-done
	if res.err != nil || res.tok != "jwt-1" {
		t.Fatalf("got %q %v, want a token from the retry", res.tok, res.err)
	}
	if calls := f.l.refreshCalls(); !slices.Equal(calls, []string{"rt-0", "rt-other"}) {
		t.Fatalf("refresh requests %v", calls)
	}
	if got := f.stored(); got.RefreshToken != "rt-1" {
		t.Fatalf("stored %+v", got)
	}
}

// invalid_grant with an unchanged store is a revoked token: the error is
// reported and nothing is retried.
func TestInvalidGrantWithUnchangedStoreFails(t *testing.T) {
	f := newRotationFixture(t)
	f.l.setValid()
	_, err := f.source()(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "ownsand login") {
		t.Fatalf("got %v", err)
	}
	if calls := f.l.refreshCalls(); len(calls) != 1 {
		t.Fatalf("refresh requests %v", calls)
	}
}

// A source that finds its session removed or replaced before it refreshes
// sends no request, and keeps failing.
func TestTokenSourceStaleBeforeRefresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *rotationFixture)
		want   string
	}{
		{"signed out", func(f *rotationFixture) {
			if err := f.st.Delete(); err != nil {
				t.Fatal(err)
			}
		}, "signed out"},
		{"signed in again", func(f *rotationFixture) {
			f.save("other", "rt-9", "jwt-9", time.Now().Add(time.Hour))
		}, "replaced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			ts := f.source()
			tc.change(f)
			for i := 0; i < 2; i++ {
				if _, err := ts(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("call %d: got %v, want %q", i, err, tc.want)
				}
			}
			if calls := f.l.refreshCalls(); len(calls) != 0 {
				t.Fatalf("refresh requests %v", calls)
			}
		})
	}
}
