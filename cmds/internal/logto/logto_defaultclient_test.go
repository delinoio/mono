// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
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

// TestRealNetworkDialTimeout runs the default client against real network
// conditions. It is opt-in, because it needs a network that drops packets to
// 10.255.255.1 (a blackhole) and waits for a real connect timeout:
//
//	LOGTO_REAL_NETWORK_PROBE=1 go test ./cmds/internal/logto -run RealNetwork -v
//
// LOGTO_REAL_NETWORK_PROBE=production waits for the production dial timeout;
// any other non-empty value uses a 1 second connect timeout.
func TestRealNetworkDialTimeout(t *testing.T) {
	mode := os.Getenv("LOGTO_REAL_NETWORK_PROBE")
	if mode == "" {
		t.Skip("set LOGTO_REAL_NETWORK_PROBE=1 to run against a real blackholed address")
	}
	dialTimeout := time.Second
	if mode == "production" {
		dialTimeout = defaultDialTimeout
	}
	client := newDefaultClient(dialTimeout)
	t.Logf("dial timeout %v, request timeout %v, production defaults %v and %v",
		dialTimeout, client.Timeout, defaultDialTimeout, defaultRequestTimeout)

	t.Run("blackhole is a dial timeout", func(t *testing.T) {
		cfg := Config{Endpoint: "http://10.255.255.1:81", AppID: "app", HTTPClient: client}
		var oe *net.OpError
		start := time.Now()
		err := cfg.post(context.Background(), "/oidc/token", nil, &Token{})
		t.Logf("after %v: %v", time.Since(start), err)
		if !errors.As(err, &oe) || !isDialTimeout(err) {
			t.Skipf("this network does not blackhole 10.255.255.1: %v", err)
		}
	})

	t.Run("PollToken backs off after the dial timeout", func(t *testing.T) {
		cfg := Config{Endpoint: "http://10.255.255.1:81", AppID: "app", HTTPClient: client}
		// Wait for the first dial timeout plus the doubled interval, then stop.
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+400*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 600}, 100*time.Millisecond)
		t.Logf("after %v: %v", time.Since(start), err)
		// The login must still be polling when the caller stops it, instead of
		// ending with the transport error after the first dial timeout.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the polling ended with %v, want it to keep polling until the caller deadline", err)
		}
	})

	t.Run("refused connection ends the login at once", func(t *testing.T) {
		cfg := Config{Endpoint: "http://127.0.0.1:1", AppID: "app", HTTPClient: client}
		start := time.Now()
		_, err := cfg.PollToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 600}, time.Millisecond)
		t.Logf("after %v: %v", time.Since(start), err)
		if err == nil || errors.Is(err, ErrExpired) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
		if time.Since(start) > dialTimeout {
			t.Fatal("the refused connection was not reported at once")
		}
	})
}
