// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultClientConnectTimeoutIsBelowTheRequestTimeout(t *testing.T) {
	if defaultDialTimeout >= defaultRequestTimeout {
		t.Fatalf("dial timeout %v is not below request timeout %v", defaultDialTimeout, defaultRequestTimeout)
	}
	c := (Config{}).client()
	if c.Timeout != defaultRequestTimeout {
		t.Fatalf("request timeout %v, want %v", c.Timeout, defaultRequestTimeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("the default transport is %T without a dial function", c.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("the default client changed the shared http.DefaultTransport")
	}
	if tr.TLSHandshakeTimeout == 0 || !tr.ForceAttemptHTTP2 || tr.Proxy == nil {
		t.Fatal("the default client lost the settings of http.DefaultTransport")
	}
	if (Config{}).client() != c {
		t.Fatal("the default client is not shared between requests")
	}
}

func TestClientKeepsACallerProvidedClient(t *testing.T) {
	own := &http.Client{}
	if (Config{HTTPClient: own}).client() != own {
		t.Fatal("a caller-provided client was replaced")
	}
}

// TestRealNetworkDialTimeout is opt-in. Use a network that drops TCP packets
// to 10.255.255.1:81 to exercise the dial timeout and retry.
//
//	LOGTO_REAL_NETWORK_PROBE=1 go test ./cmds/internal/logto -run RealNetwork -v
//
// LOGTO_REAL_NETWORK_PROBE=production uses the production dial timeout.
// Any other non-empty value uses a 1 second dial timeout.
func TestRealNetworkDialTimeout(t *testing.T) {
	mode := os.Getenv("LOGTO_REAL_NETWORK_PROBE")
	if mode == "" {
		t.Skip("set LOGTO_REAL_NETWORK_PROBE=1 to enable the real network probe")
	}
	dialTimeout := time.Second
	if mode == "production" {
		dialTimeout = defaultDialTimeout
	}
	newProbeConfig := func(t *testing.T, address string) Config {
		client := newDefaultClient(dialTimeout)
		client.Transport.(*http.Transport).Proxy = nil
		t.Cleanup(client.CloseIdleConnections)
		return Config{Endpoint: "http://" + address, AppID: "app", HTTPClient: client}
	}

	t.Run("blackhole", func(t *testing.T) {
		const address = "10.255.255.1:81"
		reference := net.Dialer{Timeout: dialTimeout}
		start := time.Now()
		conn, err := reference.DialContext(t.Context(), "tcp", address)
		elapsed := time.Since(start)
		if err == nil {
			_ = conn.Close()
			t.Skipf("reference dial to %s connected after %v", address, elapsed)
		}
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() || elapsed < dialTimeout {
			t.Skipf("reference dial to %s ended after %v, want a timeout after %v: %v",
				address, elapsed, dialTimeout, err)
		}
		t.Logf("reference dial timed out after %v: %v", elapsed, err)

		t.Run("blackhole is a dial timeout", func(t *testing.T) {
			cfg := newProbeConfig(t, address)
			ctx, cancel := context.WithTimeout(t.Context(), dialTimeout+2*time.Second)
			defer cancel()
			start := time.Now()
			err := cfg.post(ctx, "/oidc/token", nil, &Token{})
			elapsed := time.Since(start)
			t.Logf("token request ended after %v: %v", elapsed, err)
			if !isDialTimeout(err) || elapsed < dialTimeout || ctx.Err() != nil {
				t.Fatalf("want a dial timeout after %v with an active caller, got %v (caller %v)",
					dialTimeout, err, ctx.Err())
			}
		})

		t.Run("PollToken backs off after the dial timeout", func(t *testing.T) {
			testRealDialRetry(t, newProbeConfig(t, address), dialTimeout)
		})
	})

	t.Run("refused connection ends the login at once", func(t *testing.T) {
		cfg := newProbeConfig(t, "127.0.0.1:1")
		ctx, cancel := context.WithTimeout(t.Context(), dialTimeout)
		defer cancel()
		start := time.Now()
		_, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 600}, time.Millisecond)
		t.Logf("polling ended after %v: %v", time.Since(start), err)
		var dialErr *net.OpError
		if !errors.As(err, &dialErr) || dialErr.Op != "dial" || dialErr.Timeout() || ctx.Err() != nil {
			t.Fatalf("want an immediate dial failure, got %v (caller %v)", err, ctx.Err())
		}
	})
}

func testRealDialRetry(t *testing.T, cfg Config, dialTimeout time.Duration) {
	t.Helper()
	const interval = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), dialTimeout+2*time.Second)
	defer cancel()

	type dialResult struct {
		finished  time.Time
		elapsed   time.Duration
		err       error
		callerErr error
	}
	firstDial := make(chan dialResult, 1)
	retryStarted := make(chan time.Time, 1)
	var attempts atomic.Int32
	transport := cfg.HTTPClient.Transport.(*http.Transport)
	dial := transport.DialContext
	if dial == nil {
		t.Fatal("the default transport has no dial function")
	}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		start := time.Now()
		attempt := attempts.Add(1)
		if attempt == 2 {
			retryStarted <- start
			cancel()
		}
		conn, err := dial(dialCtx, network, address)
		if attempt == 1 {
			finished := time.Now()
			firstDial <- dialResult{finished, finished.Sub(start), err, ctx.Err()}
		}
		return conn, err
	}

	_, pollErr := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 600}, interval)
	var first dialResult
	select {
	case first = <-firstDial:
	default:
		t.Fatalf("polling ended before the first dial completed: %v", pollErr)
	}
	t.Logf("first dial ended after %v: %v (caller %v)", first.elapsed, first.err, first.callerErr)
	if !isDialTimeout(first.err) || first.elapsed < dialTimeout || first.callerErr != nil {
		t.Fatalf("want the first dial to time out after %v with an active caller", dialTimeout)
	}
	select {
	case start := <-retryStarted:
		delay := start.Sub(first.finished)
		t.Logf("retry started %v after the first dial ended", delay)
		if delay < 2*interval {
			t.Fatalf("retry delay %v, want at least %v", delay, 2*interval)
		}
	default:
		t.Fatalf("polling ended before a second dial started: %v", pollErr)
	}
	if !errors.Is(pollErr, context.Canceled) {
		t.Fatalf("polling ended with %v, want cancellation after the retry started", pollErr)
	}
}
