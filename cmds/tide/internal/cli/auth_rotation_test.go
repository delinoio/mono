// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
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

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/go/tide"
)

type rotationFixture struct {
	t     *testing.T
	store credstore.Store
	url   string
	mu    sync.Mutex
	calls []string
}

const rejectedRefresh = `{"error":"invalid_grant"}`

func newRotationFixture(t *testing.T, respond func(*rotationFixture, string, int) string) *rotationFixture {
	t.Helper()
	f := &rotationFixture{t: t, store: credstore.Store{Dir: t.TempDir(), Program: "tide"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oidc/token" || r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("unexpected issuer request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		refresh := r.FormValue("refresh_token")
		f.mu.Lock()
		f.calls = append(f.calls, refresh)
		attempt := len(f.calls)
		f.mu.Unlock()

		body := respond(f, refresh, attempt)
		w.Header().Set("Content-Type", "application/json")
		if body == rejectedRefresh {
			w.WriteHeader(http.StatusBadRequest)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	if err := f.save("session", "rt-0", "expired", -time.Hour); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *rotationFixture) save(session, refresh, access string, lifetime time.Duration) error {
	return f.store.Save(&credstore.Credentials{
		SessionID: session, LogtoEndpoint: f.url, AppID: "app", Resource: DefaultLogtoResource,
		RefreshToken: refresh, AccessToken: access, AccessTokenExpiry: time.Now().Add(lifetime),
	})
}

func (f *rotationFixture) source(logs io.Writer) tide.TokenSource {
	f.t.Helper()
	a := &app{
		env: Env{LookupEnv: func(k string) (string, bool) {
			if k == EnvConfigDir {
				return f.store.Dir, true
			}
			return "", false
		}},
		logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	source, err := a.tokenSource()
	if err != nil {
		f.t.Fatal(err)
	}
	return source
}

func (f *rotationFixture) assertCalls(want ...string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.calls, want) {
		f.t.Fatalf("issuer refreshes = %v, want %v", f.calls, want)
	}
}

func (f *rotationFixture) assertStored(refresh, access string) {
	f.t.Helper()
	got, err := f.store.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	if got.SessionID != "session" || got.RefreshToken != refresh || got.AccessToken != access {
		f.t.Fatalf("stored session, refresh and access = %q, %q, %q, want session, %q, %q",
			got.SessionID, got.RefreshToken, got.AccessToken, refresh, access)
	}
}

func tokenResponse(access, refresh string, seconds int) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":%d}`, access, refresh, seconds)
}

func assertAccessToken(t *testing.T, source tide.TokenSource, want string) {
	t.Helper()
	if got, err := source(t.Context()); err != nil || got != want {
		t.Fatalf("access token = %q, error = %v, want %q", got, err, want)
	}
}

func TestTokenSourceAdoptsStoredRotation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lifetime int
		want     string
		refresh  string
		calls    []string
	}{
		{"access token is fresh", 3600, "jwt-1", "rt-1", []string{"rt-0"}},
		{"access token needs refresh", 1, "jwt-2", "rt-2", []string{"rt-0", "rt-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, func(_ *rotationFixture, refresh string, attempt int) string {
				switch {
				case refresh == "rt-0" && attempt == 1:
					return tokenResponse("jwt-1", "rt-1", tc.lifetime)
				case refresh == "rt-1" && attempt == 2:
					return tokenResponse("jwt-2", "rt-2", 3600)
				default:
					return rejectedRefresh
				}
			})
			a, b := f.source(io.Discard), f.source(io.Discard)
			assertAccessToken(t, a, "jwt-1")
			assertAccessToken(t, b, tc.want)
			f.assertCalls(tc.calls...)
			f.assertStored(tc.refresh, tc.want)
		})
	}
}

func TestTokenSourceDiscardsSupersededRefresh(t *testing.T) {
	f := newRotationFixture(t, func(f *rotationFixture, _ string, _ int) string {
		// Another process publishes while the original request is in flight.
		if err := f.save("session", "rt-newer", "jwt-newer", time.Hour); err != nil {
			t.Error(err)
		}
		return tokenResponse("jwt-stale", "rt-stale", 3600)
	})
	assertAccessToken(t, f.source(io.Discard), "jwt-newer")
	f.assertStored("rt-newer", "jwt-newer")
	f.assertCalls("rt-0")
}

func TestTokenSourceRetriesInvalidGrant(t *testing.T) {
	for _, timing := range []string{"before rejection", "after rejection"} {
		t.Run(timing, func(t *testing.T) {
			publish := func(f *rotationFixture) {
				if err := f.save("session", "rt-other", "expired", -time.Hour); err != nil {
					t.Error(err)
				}
			}
			f := newRotationFixture(t, func(f *rotationFixture, refresh string, attempt int) string {
				switch {
				case refresh == "rt-other" && attempt == 2:
					return tokenResponse("jwt-newer", "rt-newer", 3600)
				case timing == "before rejection":
					publish(f)
				}
				return rejectedRefresh
			})
			logs := rotationWaitWriter(func() {
				if timing == "after rejection" {
					publish(f)
				}
			})
			assertAccessToken(t, f.source(logs), "jwt-newer")
			f.assertCalls("rt-0", "rt-other")
			f.assertStored("rt-newer", "jwt-newer")
		})
	}
}

// rotationWaitWriter acts when the source starts waiting after the first
// reload still found the rejected token.
type rotationWaitWriter func()

func (w rotationWaitWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "waits for another process to publish the rotated Logto refresh token") {
		w()
	}
	return len(p), nil
}

func TestTokenSourceInvalidGrantWithUnchangedStore(t *testing.T) {
	f := newRotationFixture(t, func(*rotationFixture, string, int) string { return rejectedRefresh })
	source := f.source(io.Discard)
	ctx, cancel := context.WithTimeout(t.Context(), refreshAdoptWait+2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := source(ctx)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "tide login") {
		t.Fatalf("got %v, want invalid_grant with tide login guidance", err)
	}
	if elapsed := time.Since(start); elapsed < refreshAdoptWait {
		t.Fatalf("returned after %v, before waiting %v", elapsed, refreshAdoptWait)
	}
	f.assertCalls("rt-0")
}

func TestTokenSourceStopsWaitingOnCancel(t *testing.T) {
	f := newRotationFixture(t, func(*rotationFixture, string, int) string { return rejectedRefresh })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := f.source(rotationWaitWriter(cancel))(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
	f.assertCalls("rt-0")
}

func TestTokenSourceRejectsChangedSession(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(f *rotationFixture) error
		want  string
	}{
		{"logout", func(f *rotationFixture) error { return f.store.Delete() },
			"the stored credentials were removed while the Logto token refreshed (signed out); run 'tide login' to sign in again"},
		{"login", func(f *rotationFixture) error { return f.save("new-session", "rt-new", "jwt-new", time.Hour) },
			"the stored credentials were replaced while the Logto token refreshed (signed in again); run the command again to load them"},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newRotationFixture(t, func(f *rotationFixture, _ string, _ int) string {
				if err := change.apply(f); err != nil {
					t.Error(err)
				}
				return tokenResponse("jwt-stale", "rt-stale", 3600)
			})
			source := f.source(io.Discard)
			for range 2 {
				if _, err := source(t.Context()); err == nil || err.Error() != change.want {
					t.Fatalf("got %v, want %q", err, change.want)
				}
			}
			f.assertCalls("rt-0")
			stored, err := f.store.Load()
			if change.name == "logout" && !errors.Is(err, credstore.ErrNotFound) {
				t.Fatalf("after logout, store load error = %v, want missing credentials", err)
			}
			if change.name == "login" && (err != nil || stored.SessionID != "new-session" || stored.RefreshToken != "rt-new") {
				t.Fatalf("new session was lost: credentials = %+v, error = %v", stored, err)
			}
		})
	}
}

func TestTokenSourceLimitsRefreshAttempts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"rejected refreshes", rejectedRefresh},
		{"superseded responses", tokenResponse("jwt-stale", "rt-stale", 3600)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, func(f *rotationFixture, _ string, attempt int) string {
				if err := f.save("session", fmt.Sprintf("rt-%d", attempt), "expired", -time.Hour); err != nil {
					t.Error(err)
				}
				return tc.body
			})
			if _, err := f.source(io.Discard)(t.Context()); err == nil {
				t.Fatal("refresh succeeded despite every result being superseded")
			}
			f.assertCalls("rt-0", "rt-1", "rt-2")
			f.assertStored("rt-3", "expired")
		})
	}
}
