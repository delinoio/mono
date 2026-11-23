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
// credentials when it is created. The wait for a rotation that another process
// has not published yet is short, so that a revoked token fails quickly.
func (f *rotationFixture) source() func(context.Context) (string, error) {
	f.t.Helper()
	return f.sourceWith(50*time.Millisecond, 5*time.Millisecond, io.Discard)
}

// sourceWith is source with the bound and the poll interval of the wait for a
// rotation and the writer that receives the debug log of the source.
func (f *rotationFixture) sourceWith(wait, poll time.Duration, logs io.Writer) func(context.Context) (string, error) {
	f.t.Helper()
	a := &app{
		env: Env{
			LookupEnv:        func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
			RefreshAdoptWait: wait,
			RefreshAdoptPoll: poll,
		},
		logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	ts, err := a.tokenSource()
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

// rotationWait bounds every wait of a rotation test, so that a regression
// fails the test promptly instead of hanging it until the package timeout.
const rotationWait = 10 * time.Second

// signalWriter closes signal when a write contains the marker. The source
// logs that marker when it starts to wait for a rotation, so a test can act
// during the wait instead of guessing with a sleep.
type signalWriter struct {
	marker string
	once   sync.Once
	signal chan struct{}
}

func newSignalWriter(marker string) *signalWriter {
	return &signalWriter{marker: marker, signal: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		w.once.Do(func() { close(w.signal) })
	}
	return len(p), nil
}

// awaitWaiting waits until the source logged that it waits for a rotation.
func awaitWaiting(t *testing.T, w *signalWriter, done <-chan tokenResult) {
	t.Helper()
	select {
	case <-w.signal:
	case res := <-done:
		t.Fatalf("the token request ended without waiting for a rotation: token %q, error %v", res.tok, res.err)
	case <-time.After(rotationWait):
		t.Fatal("the source did not wait for a rotation")
	}
}

// tokenResult is the outcome of one token request.
type tokenResult struct {
	tok string
	err error
}

// hold makes the issuer pause every request whose refresh token match
// accepts, until release runs. started closes when the first such request
// arrived. release is idempotent and is also registered as a cleanup that
// runs before the httptest.Server cleanup of the fixture (cleanups run in
// reverse order): the server waits for in-flight handlers, so a held handler
// would hang the cleanup when a test fails before it releases the request.
func (f *rotationFixture) hold(match func(refreshToken string) bool) (started <-chan struct{}, release func()) {
	f.t.Helper()
	startedCh, releaseCh := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	f.l.mu.Lock()
	f.l.hook = func(rt string) {
		if !match(rt) {
			return
		}
		startOnce.Do(func() { close(startedCh) })
		<-releaseCh
	}
	f.l.mu.Unlock()
	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	f.t.Cleanup(release)
	return startedCh, release
}

// tokenAsync requests a token on a worker goroutine. The worker only runs
// the token source: it must not call t.Fatal, which is for the test goroutine.
func tokenAsync(ctx context.Context, ts func(context.Context) (string, error)) <-chan tokenResult {
	done := make(chan tokenResult, 1)
	go func() {
		tok, err := ts(ctx)
		done <- tokenResult{tok, err}
	}()
	return done
}

// awaitHeld waits until the issuer holds a request. A worker that finished
// before that (for example with an error) or a missing request fails the test
// at once, with the result of the worker.
func awaitHeld(t *testing.T, started <-chan struct{}, done <-chan tokenResult) {
	t.Helper()
	select {
	case <-started:
	case res := <-done:
		t.Fatalf("the token request ended before the issuer held a refresh request: token %q, error %v", res.tok, res.err)
	case <-time.After(rotationWait):
		t.Fatal("the issuer received no refresh request")
	}
}

// awaitResult waits for the outcome of a token request.
func awaitResult(t *testing.T, done <-chan tokenResult) tokenResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(rotationWait):
		t.Fatal("the token request did not finish")
		return tokenResult{}
	}
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
	started, release := f.hold(func(string) bool { return true })
	// The source is created on the test goroutine: it calls t.Fatal on error.
	ts := f.source()
	done := tokenAsync(context.Background(), ts)
	awaitHeld(t, started, done)
	// Another process refreshed and stored its rotation meanwhile. The held
	// request is released even when the save fails.
	f.save("s", "rt-newer", "newer-jwt", time.Now().Add(time.Hour))
	release()
	res := awaitResult(t, done)
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
	started, release := f.hold(func(rt string) bool { return rt == "rt-0" })
	ts := f.source()
	done := tokenAsync(context.Background(), ts)
	awaitHeld(t, started, done)
	// Another process rotated rt-0 to rt-other; its access token is expired.
	f.l.setValid("rt-other")
	f.save("s", "rt-other", "expired", time.Now().Add(-time.Hour))
	release()
	res := awaitResult(t, done)
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

// waitMarker is the debug message that a source logs when it starts to wait
// for another process to publish a rotated refresh token.
const waitMarker = "waits for another process to publish the rotated Logto refresh token"

// Two processes send the same refresh token. The issuer rotates it for the
// first one, so it rejects the second with invalid_grant before the first one
// published the rotation. The second source waits for the publication and
// adopts it instead of reporting a revoked token.
func TestInvalidGrantWaitsForPendingRotation(t *testing.T) {
	f := newRotationFixture(t)
	started, release := f.hold(func(rt string) bool { return rt == "rt-0" })
	logs := newSignalWriter(waitMarker)
	ts := f.sourceWith(rotationWait, 5*time.Millisecond, logs)
	done := tokenAsync(context.Background(), ts)
	awaitHeld(t, started, done)
	// The first process got rt-0 rotated to rt-1 and has not stored it yet.
	f.l.setValid("rt-1")
	release()
	awaitWaiting(t, logs, done)
	// The first process publishes its rotation, with a valid access token.
	f.save("s", "rt-1", "published-jwt", time.Now().Add(time.Hour))
	res := awaitResult(t, done)
	if res.err != nil || res.tok != "published-jwt" {
		t.Fatalf("got %q %v, want the published token", res.tok, res.err)
	}
	if calls := f.l.refreshCalls(); !slices.Equal(calls, []string{"rt-0"}) {
		t.Fatalf("refresh requests %v, want only the rejected one", calls)
	}
}

// The published rotation may need a refresh again: the source then retries
// with the published refresh token.
func TestInvalidGrantWaitRetriesWithPublishedToken(t *testing.T) {
	f := newRotationFixture(t)
	started, release := f.hold(func(rt string) bool { return rt == "rt-0" })
	logs := newSignalWriter(waitMarker)
	ts := f.sourceWith(rotationWait, 5*time.Millisecond, logs)
	done := tokenAsync(context.Background(), ts)
	awaitHeld(t, started, done)
	f.l.setValid("rt-1")
	release()
	awaitWaiting(t, logs, done)
	f.save("s", "rt-1", "expired", time.Now().Add(-time.Hour))
	res := awaitResult(t, done)
	if res.err != nil || res.tok != "jwt-1" {
		t.Fatalf("got %q %v, want a token from the retry", res.tok, res.err)
	}
	if calls := f.l.refreshCalls(); !slices.Equal(calls, []string{"rt-0", "rt-1"}) {
		t.Fatalf("refresh requests %v", calls)
	}
}

// A revoked token is reported after the bound of the wait.
func TestInvalidGrantWaitEndsAtTheBound(t *testing.T) {
	f := newRotationFixture(t)
	f.l.setValid()
	const bound = 150 * time.Millisecond
	ts := f.sourceWith(bound, 5*time.Millisecond, io.Discard)
	start := time.Now()
	done := tokenAsync(context.Background(), ts)
	res := awaitResult(t, done)
	if res.err == nil || !strings.Contains(res.err.Error(), "invalid_grant") || !strings.Contains(res.err.Error(), "ownsand login") {
		t.Fatalf("got %q %v", res.tok, res.err)
	}
	if elapsed := time.Since(start); elapsed < bound {
		t.Fatalf("the error came after %v, before the %v bound", elapsed, bound)
	}
	if calls := f.l.refreshCalls(); len(calls) != 1 {
		t.Fatalf("refresh requests %v", calls)
	}
}

// Cancelling the request ends the wait at once, long before its bound.
func TestInvalidGrantWaitStopsOnCancel(t *testing.T) {
	f := newRotationFixture(t)
	f.l.setValid()
	logs := newSignalWriter(waitMarker)
	ts := f.sourceWith(time.Hour, 5*time.Millisecond, logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := tokenAsync(ctx, ts)
	awaitWaiting(t, logs, done)
	cancel()
	res := awaitResult(t, done)
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("got %q %v, want context.Canceled", res.tok, res.err)
	}
}

// A logout or a newer login during the wait keeps its own error, and the
// source keeps returning it.
func TestInvalidGrantWaitSeesLogoutAndNewLogin(t *testing.T) {
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
			f.l.setValid()
			logs := newSignalWriter(waitMarker)
			ts := f.sourceWith(rotationWait, 5*time.Millisecond, logs)
			done := tokenAsync(context.Background(), ts)
			awaitWaiting(t, logs, done)
			tc.change(f)
			res := awaitResult(t, done)
			if res.err == nil || !strings.Contains(res.err.Error(), tc.want) {
				t.Fatalf("got %q %v, want %q", res.tok, res.err, tc.want)
			}
			if _, err := ts(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("second call: got %v, want %q", err, tc.want)
			}
			if calls := f.l.refreshCalls(); len(calls) != 1 {
				t.Fatalf("refresh requests %v", calls)
			}
		})
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
