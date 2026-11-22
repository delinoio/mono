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
