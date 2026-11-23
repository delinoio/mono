// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"
)

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// dialRecorder is an http.Client transport that fails the first failures
// connection attempts with err and then connects to the real server. It
// disables keep-alive, so every request dials, and records when each dial
// started. onDial, when set, runs at the end of each failed dial.
type dialRecorder struct {
	mu       sync.Mutex
	starts   []time.Time
	failures int
	err      error
	onDial   func(n int)
}

func (d *dialRecorder) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.starts = append(d.starts, time.Now())
	n := len(d.starts)
	d.mu.Unlock()
	if n <= d.failures {
		if d.onDial != nil {
			d.onDial(n)
		}
		return nil, d.err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, addr)
}

func (d *dialRecorder) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: d.dialContext, DisableKeepAlives: true}}
}

func (d *dialRecorder) dials() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.starts...)
}

func dialTimeout() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
}

// requireGaps checks that dial i+1 started at least min[i] after dial i.
// Timers never fire early, so lower bounds do not depend on the timer
// granularity or on the load of the machine.
func requireGaps(t *testing.T, starts []time.Time, min ...time.Duration) {
	t.Helper()
	if len(starts) != len(min)+1 {
		t.Fatalf("%d dials, want %d", len(starts), len(min)+1)
	}
	for i, m := range min {
		if gap := starts[i+1].Sub(starts[i]); gap < m {
			t.Fatalf("dials %d and %d are %v apart, want at least %v", i+1, i+2, gap, m)
		}
	}
}

func tokenServer(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := min(n, len(responses)-1)
		n++
		mu.Unlock()
		if responses[i] == "" {
			fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, responses[i])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPollTokenDialTimeoutDoublesTheIntervalEachTime(t *testing.T) {
	srv := tokenServer(t, "")
	rec := &dialRecorder{failures: 2, err: dialTimeout()}
	cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: rec.client()}
	const base = 50 * time.Millisecond
	tok, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, base)
	if err != nil || tok == nil || tok.RefreshToken != "r" {
		t.Fatalf("%v %v", tok, err)
	}
	// The first wait is the base interval, then it doubles after each timeout.
	requireGaps(t, rec.dials(), 2*base, 4*base)
}

func TestPollTokenDialTimeoutIntervalIsKeptForLaterPolls(t *testing.T) {
	srv := tokenServer(t, `{"error":"authorization_pending"}`, "")
	rec := &dialRecorder{failures: 1, err: dialTimeout()}
	cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: rec.client()}
	const base = 50 * time.Millisecond
	tok, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, base)
	if err != nil || tok == nil {
		t.Fatalf("%v %v", tok, err)
	}
	// dial 1 times out, request 2 is authorization_pending, request 3 succeeds.
	requireGaps(t, rec.dials(), 2*base, 2*base)
}

func TestPollTokenDialTimeoutExpiryDuringBackoffSendsNoRequest(t *testing.T) {
	srv := tokenServer(t, "")
	rec := &dialRecorder{failures: 10, err: dialTimeout()}
	cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: rec.client()}
	// The first dial happens after 400 ms; the doubled interval puts the next
	// one at 1200 ms, past the code lifetime of 1 second.
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 1}, 400*time.Millisecond)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if n := len(rec.dials()); n != 1 {
		t.Fatalf("%d dials, want 1", n)
	}
}

func TestPollTokenDialTimeoutCallerCancelDuringBackoffReturnsCallerError(t *testing.T) {
	srv := tokenServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &dialRecorder{failures: 10, err: dialTimeout(), onDial: func(int) { cancel() }}
	cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: rec.client()}
	_, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if n := len(rec.dials()); n != 1 {
		t.Fatalf("%d dials, want 1", n)
	}
}

func TestPollTokenOtherTransportFailuresEndThePolling(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}
	for name, failure := range map[string]error{
		"connection refused":     refused,
		"non-timeout dial error": errors.New("dial failed"),
		"read timeout":           readTimeout,
	} {
		t.Run(name, func(t *testing.T) {
			srv := tokenServer(t, "")
			rec := &dialRecorder{failures: 10, err: failure}
			cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: rec.client()}
			_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
			if err == nil || errors.Is(err, ErrExpired) || errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
			if n := len(rec.dials()); n != 1 {
				t.Fatalf("%d dials, want 1", n)
			}
		})
	}
}

func TestPollTokenResponseTimeoutEndsThePolling(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	// The connection is made; only the response takes longer than the client
	// timeout, which is not a connection-establishment timeout.
	cfg := Config{Endpoint: srv.URL, AppID: "app", HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}}
	_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("%d requests, want 1", requests)
	}
}

func TestIsDialTimeout(t *testing.T) {
	wrapped := fmt.Errorf("logto: /oidc/token: %w", &net.OpError{Op: "dial", Err: timeoutError{}})
	if !isDialTimeout(wrapped) {
		t.Fatal("a wrapped dial timeout was not detected")
	}
	if isDialTimeout(errors.New("x")) || isDialTimeout(context.DeadlineExceeded) {
		t.Fatal("a non-dial error was detected as a dial timeout")
	}
}
