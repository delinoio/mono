// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollTokenStates(t *testing.T) {
	for _, tc := range []struct {
		responses []string
		want      error
	}{
		{[]string{"slow_down", "authorization_pending"}, nil},
		{[]string{"access_denied"}, ErrAccessDenied},
		{[]string{"expired_token"}, ErrExpired},
	} {
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n < len(tc.responses) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, tc.responses[n])
				n++
				return
			}
			fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
		}))
		cfg := Config{Endpoint: srv.URL, AppID: "app"}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		tok, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
		cancel()
		srv.Close()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%v: got %v", tc.responses, err)
			}
			continue
		}
		if err != nil || tok.RefreshToken != "r" {
			t.Errorf("%v: %v %v", tc.responses, tok, err)
		}
	}
}

// The expiry tests use short device code lifetimes and wide margins, so that
// they stay stable on slow runners with coarse timers. A bug shows up as a
// wait of 5 seconds or more (the interval) or a request that outlives the code.
const expiryUpperBound = 4 * time.Second

func TestPollTokenExpiresDuringIntervalWait(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	start := time.Now()
	// The code lives 1 second, the interval is 5 seconds.
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 1, Interval: 5}, 0)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > expiryUpperBound {
		t.Fatalf("expired after %v, want about 1s", elapsed)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d token requests after the code expired", n)
	}
}

func TestPollTokenCancelsPendingRequestAtDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a closed connection only after the handler
		// has read the request body.
		_ = r.ParseForm()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	start := time.Now()
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 1}, 10*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > expiryUpperBound {
		t.Fatalf("expired after %v, want about 1s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("the token request never started")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending token request was not cancelled")
	}
}

func TestPollTokenSlowDownPastDeadlineSendsNoRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"slow_down"}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	start := time.Now()
	// slow_down moves the next request about 5 seconds out, past the code
	// lifetime of 2 seconds.
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 2}, 10*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed < 1900*time.Millisecond || elapsed > 2*time.Second+expiryUpperBound {
		t.Fatalf("expired after %v, want about 2s", elapsed)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d token requests, want exactly the one before slow_down", n)
	}
}

func TestPollTokenCallerEndsStayDistinctFromExpiry(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	da := &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60, Interval: 5}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := cfg.PollToken(ctx, da, 0); !errors.Is(err, context.Canceled) || errors.Is(err, ErrExpired) {
		t.Fatalf("caller cancellation: got %v", err)
	}
	// A caller deadline shorter than the code lifetime is not an expiry.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := cfg.PollToken(ctx, da, 0); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrExpired) {
		t.Fatalf("caller deadline: got %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d token requests while the caller had cancelled", n)
	}
}

func TestPollTokenTransportFailureBeforeDeadlineIsNotExpiry(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()
	cfg := Config{Endpoint: endpoint, AppID: "app"}
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
	if err == nil || errors.Is(err, ErrExpired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

// The expiry deadline bounds only the device grant: the caller context stays
// usable for the refresh grant after the code lifetime has passed.
func TestPollTokenDeadlineDoesNotBoundTheCallerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tok, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 1}, time.Millisecond)
	if err != nil || tok.RefreshToken != "r" {
		t.Fatalf("%v %v", tok, err)
	}
	time.Sleep(1200 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("the caller context ended: %v", ctx.Err())
	}
	if _, err := cfg.Refresh(ctx, tok.RefreshToken); err != nil {
		t.Fatalf("refresh after the code lifetime: %v", err)
	}
}

// A device authorization without a positive expires_in falls back to a
// 10-minute lifetime; it must not expire at once.
func TestPollTokenWithoutExpiresInKeepsPolling(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tok, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 0}, time.Millisecond)
	if err != nil || tok.RefreshToken != "r" || requests.Load() != 2 {
		t.Fatalf("%v %v after %d requests", tok, err, requests.Load())
	}
}

// StartDeviceAuthorization records the local expiry deadline relative to the
// time of the response: expires_in seconds later, or 10 minutes later when
// expires_in is not positive.
func TestStartDeviceAuthorizationRecordsResponseRelativeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn int
		lifetime  time.Duration
	}{
		{"positive", 90, 90 * time.Second},
		{"missing", 0, 10 * time.Minute},
		{"negative", -5, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"device_code":"d","user_code":"u","verification_uri":"https://login.example","expires_in":%d}`, tc.expiresIn)
			}))
			defer srv.Close()
			before := time.Now()
			da, err := Config{Endpoint: srv.URL, AppID: "app"}.StartDeviceAuthorization(context.Background())
			after := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			if da.ExpiresAt.Before(before.Add(tc.lifetime)) || da.ExpiresAt.After(after.Add(tc.lifetime)) {
				t.Fatalf("ExpiresAt %v, want between %v and %v", da.ExpiresAt, before.Add(tc.lifetime), after.Add(tc.lifetime))
			}
		})
	}
}

// A device authorization whose deadline passed before the polling started
// returns ErrExpired at once and sends no token request.
func TestPollTokenAlreadyExpiredSendsNoRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	start := time.Now()
	da := &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60, ExpiresAt: time.Now().Add(-time.Second)}
	_, err := cfg.PollToken(context.Background(), da, time.Millisecond)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %v, want at once", elapsed)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d token requests for an expired authorization", n)
	}
}

// Time that passes between the device authorization response and the start of
// the polling (the caller blocks while it writes the sign-in prompt) counts
// against the code lifetime.
func TestPollTokenCountsTimeSinceAuthorizationResponse(t *testing.T) {
	var tokenRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oidc/device/auth" {
			fmt.Fprint(w, `{"device_code":"d","user_code":"u","verification_uri":"https://login.example","expires_in":1,"interval":0}`)
			return
		}
		tokenRequests.Add(1)
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	da, err := cfg.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The code lives 1 second; the caller is blocked for longer.
	time.Sleep(1200 * time.Millisecond)
	start := time.Now()
	_, err = cfg.PollToken(context.Background(), da, time.Millisecond)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %v, want at once", elapsed)
	}
	if n := tokenRequests.Load(); n != 0 {
		t.Fatalf("%d token requests after the code expired", n)
	}
}

// The response-relative deadline cancels a token request that is still in
// flight, even though the polling started well before it.
func TestPollTokenCancelsPendingRequestAtResponseRelativeDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	start := time.Now()
	// The authorization response arrived 2 seconds ago; the code lives 3.
	da := &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 3, ExpiresAt: start.Add(time.Second)}
	_, err := cfg.PollToken(context.Background(), da, 10*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > expiryUpperBound {
		t.Fatalf("expired after %v, want about 1s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("the token request never started")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending token request was not cancelled")
	}
}

// An earlier cancellation or deadline of the caller still returns the caller
// error, also when the authorization is already expired or a deadline is set.
func TestPollTokenCallerEndsWinOverResponseRelativeDeadline(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		at   time.Time
		want error
	}{
		{"cancelled before an expired deadline", cancelled, time.Now().Add(-time.Second), context.Canceled},
		{"cancelled before a future deadline", cancelled, time.Now().Add(time.Minute), context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cfg.PollToken(tc.ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60, ExpiresAt: tc.at}, time.Millisecond)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrExpired) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("caller deadline before the code deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60, ExpiresAt: time.Now().Add(time.Minute)}, 10*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrExpired) {
			t.Fatalf("got %v, want the caller deadline", err)
		}
	})
}

// A device grant that succeeds before the response-relative deadline returns
// the token, and the caller context stays usable afterwards.
func TestPollTokenSucceedsBeforeResponseRelativeDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
	}))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL, AppID: "app"}
	ctx := context.Background()
	da := &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60, ExpiresAt: time.Now().Add(time.Minute)}
	tok, err := cfg.PollToken(ctx, da, time.Millisecond)
	if err != nil || tok.RefreshToken != "r" {
		t.Fatalf("got %+v, %v", tok, err)
	}
	if _, err := cfg.Refresh(ctx, tok.RefreshToken); err != nil {
		t.Fatalf("the refresh grant after the device grant failed: %v", err)
	}
}

func TestErrorsNeverContainTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"grant request is invalid"}`)
	}))
	defer srv.Close()
	_, err := Config{Endpoint: srv.URL, AppID: "app"}.Refresh(context.Background(), "refresh-secret")
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("err %v", err)
	}
	if got := err.Error(); got == "" || strings.Contains(got, "refresh-secret") {
		t.Fatalf("error %q", got)
	}
}

func TestValidate(t *testing.T) {
	if (Config{}).Validate() == nil || (Config{Endpoint: "ftp://x", AppID: "a"}).Validate() == nil {
		t.Fatal("invalid configuration accepted")
	}
	if err := (Config{Endpoint: "https://auth.example", AppID: "a"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
