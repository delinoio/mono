// SPDX-License-Identifier: FSL-1.1-ALv2

package cpclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func TestHTTPClientRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		target string
		hops   int
		err    string
	}{
		{name: "same origin", origin: "https://agent.test", target: "https://agent.test/next", hops: 9},
		{name: "explicit HTTPS default", origin: "https://agent.test", target: "https://AGENT.test:443/next", hops: 1},
		{name: "implicit HTTP default", origin: "http://agent.test:80", target: "http://agent.test/next", hops: 1},
		{name: "same custom port", origin: "https://agent.test:8443", target: "https://agent.test:8443/next", hops: 1},
		{name: "zero-padded default", origin: "https://agent.test", target: "https://agent.test:0443/next", hops: 1},
		{name: "limit", origin: "https://agent.test", target: "https://agent.test/next", hops: 10, err: "stopped after 10 redirects"},
		{name: "different scheme", origin: "https://agent.test", target: "http://agent.test:443/next", hops: 1, err: "redirect leaves the control plane origin"},
		{name: "different hostname", origin: "https://agent.test", target: "https://other.agent.test/next", hops: 2, err: "redirect leaves the control plane origin"},
		{name: "different port", origin: "https://agent.test", target: "https://agent.test:8443/next", hops: 1, err: "redirect leaves the control plane origin"},
		{name: "different IPv6 zone", origin: "http://[fe80::1%25link]", target: "http://[fe80::1%25LINK]/next", hops: 1, err: "redirect leaves the control plane origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewHTTPClient(tc.origin, nil)
			if err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			err = client.CheckRedirect(&http.Request{URL: target}, make([]*http.Request, tc.hops))
			if tc.err == "" {
				if err != nil {
					t.Fatalf("CheckRedirect() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("CheckRedirect() = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestNewHTTPClientRejectsInvalidOrigin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		reason string
	}{
		{"HTTPS without authority", "https:///control", "requires a hostname"},
		{"HTTP without authority", "http:///control", "requires a hostname"},
		{"HTTPS with port only", "https://:443", "requires a hostname"},
		{"HTTP with port only", "http://:18300", "requires a hostname"},
		{"malformed URL", "https://example.test:bad-port/control", "invalid control plane origin URL"},
		{"unsupported scheme", "ftp://example.test", "must use https or http"},
		{"query", "https://agent.example.test?source=setup", "must not include a query"},
		{"empty query", "https://agent.example.test?", "must not include a query"},
		{"fragment", "https://agent.example.test#setup", "must not include a fragment"},
		{"empty fragment", "https://agent.example.test#", "must not include a fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewHTTPClient(tc.origin, nil)
			if client != nil || err == nil || !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), tc.origin) {
				t.Fatalf("NewHTTPClient() = %v, %v; want error containing %q without the URL", client, err, tc.reason)
			}
		})
	}
}

func TestNewHTTPClientSelectsHTTP2Transport(t *testing.T) {
	for _, tc := range []struct {
		origin string
		useTLS bool
	}{
		{"https://example.test", true},
		{"http://127.0.0.1:18300", false},
		{"http://[::1]:18300", false},
		{"https://example.test:8443/control%3F%23/", true},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			client, err := NewHTTPClient(tc.origin, nil)
			if err != nil {
				t.Fatal(err)
			}
			protocols := client.Transport.(*http.Transport).Protocols
			if protocols.HTTP1() || protocols.HTTP2() != tc.useTLS || protocols.UnencryptedHTTP2() == tc.useTLS {
				t.Fatalf("protocols = %v; want only HTTP/2 with TLS enabled = %t", protocols, tc.useTLS)
			}
		})
	}
}

// authTestTimeout is the shortened attempt deadline of these tests. The
// production default is DefaultAuthTimeout.
const authTestTimeout = 300 * time.Millisecond

// authTestSlack is how far below authTestTimeout an attempt may end. The
// deadline timer and the test's own clock readings are separate clocks: on
// Windows the coarse timer granularity let an attempt end about 0.3 ms before
// the measured 300 ms. The slack keeps the lower bound a check that the
// attempt waited for its deadline instead of failing fast, without asserting
// timer precision. Keep it well under authTestTimeout.
const authTestSlack = 50 * time.Millisecond

// authTestMaxElapsed bounds fixture waits without testing timer precision.
const authTestMaxElapsed = 10 * time.Second

// enrolled returns a client with a short attempt deadline and a device that
// the fake control plane knows.
func enrolled(t *testing.T) (*testcp.Server, *Client, string, *identity.Key) {
	t.Helper()
	cp := testcp.New(t)
	c, err := New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthTimeout = authTestTimeout
	key, _, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), authTestMaxElapsed)
	defer cancel()
	resp, err := c.Agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: testcp.Token,
		PublicKey:       key.PublicKey(),
		Name:            "host",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return cp, c, resp.Msg.GetDeviceId(), key
}

func TestDefaultAuthTimeout(t *testing.T) {
	if got := (&Client{}).authTimeout(); got != DefaultAuthTimeout {
		t.Fatalf("zero AuthTimeout gives %s, want %s", got, DefaultAuthTimeout)
	}
	if got := (&Client{AuthTimeout: time.Second}).authTimeout(); got != time.Second {
		t.Fatalf("AuthTimeout 1s gives %s", got)
	}
}

type authRPCStub struct {
	ownsandv1connect.AgentServiceClient
	step    string
	failure func(context.Context) error
}

func (s authRPCStub) CreateChallenge(ctx context.Context, _ *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	if s.step == "create challenge" {
		return nil, s.failure(ctx)
	}
	return connect.NewResponse(&ownsandv1.CreateChallengeResponse{Nonce: []byte("test nonce")}), nil
}

func (s authRPCStub) Authenticate(ctx context.Context, _ *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	return nil, s.failure(ctx)
}

func (s authRPCStub) ConfirmRemoval(ctx context.Context, _ *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	return nil, s.failure(ctx)
}

func TestAuthRPCDeadlineAttribution(t *testing.T) {
	key, _, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const remoteMessage = "control plane ended the RPC"
	const agentTimeout = 20 * time.Second
	for _, step := range []string{"create challenge", "authenticate", "confirm removal"} {
		t.Run(step, func(t *testing.T) {
			remoteErr := connect.NewError(connect.CodeDeadlineExceeded, errors.New(remoteMessage))
			c := &Client{
				Agent:       authRPCStub{step: step, failure: func(context.Context) error { return remoteErr }},
				AuthTimeout: agentTimeout,
			}
			call := func(ctx context.Context) error {
				if step == "confirm removal" {
					return c.ConfirmRemoval(ctx, "test-device", key)
				}
				_, err := c.Authenticate(ctx, "test-device", key, "")
				return err
			}

			t.Run("caller deadline", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := call(ctx)
				if ctx.Err() != nil {
					t.Fatalf("caller context ended early: %v", ctx.Err())
				}
				want := "cpclient: " + step + ": deadline_exceeded: " + remoteMessage
				if !errors.Is(err, remoteErr) || connect.CodeOf(err) != connect.CodeDeadlineExceeded || err.Error() != want {
					t.Fatalf("call error = %v, want %q wrapping the original Connect error", err, want)
				}
			})

			t.Run("agent timeout", func(t *testing.T) {
				err := call(context.Background())
				want := "cpclient: " + step + ": no answer within " + agentTimeout.String()
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), want) {
					t.Fatalf("call error = %v, want %q wrapping context.DeadlineExceeded", err, want)
				}
			})
		})
	}
}

func TestAuthenticateKeepsExpiredCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c := &Client{Agent: authRPCStub{
		step: "create challenge",
		failure: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}}
	_, err := c.Authenticate(ctx, "test-device", nil, "")
	want := "cpclient: create challenge: context deadline exceeded"
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != want {
		t.Fatalf("Authenticate error = %v, want %q wrapping the caller context error", err, want)
	}
}

type recordedAuthRPC struct {
	procedure   string
	deadline    time.Time
	hasDeadline bool
}

type authRPCRecorder struct {
	ownsandv1connect.AgentServiceClient
	calls []recordedAuthRPC
}

func (r *authRPCRecorder) record(ctx context.Context, procedure string) {
	deadline, hasDeadline := ctx.Deadline()
	r.calls = append(r.calls, recordedAuthRPC{procedure, deadline, hasDeadline})
}

func (r *authRPCRecorder) CreateChallenge(ctx context.Context, req *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	r.record(ctx, ownsandv1connect.AgentServiceCreateChallengeProcedure)
	return r.AgentServiceClient.CreateChallenge(ctx, req)
}

func (r *authRPCRecorder) Authenticate(ctx context.Context, req *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	r.record(ctx, ownsandv1connect.AgentServiceAuthenticateProcedure)
	return r.AgentServiceClient.Authenticate(ctx, req)
}

func (r *authRPCRecorder) ConfirmRemoval(ctx context.Context, req *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	r.record(ctx, ownsandv1connect.AgentServiceConfirmRemovalProcedure)
	return r.AgentServiceClient.ConfirmRemoval(ctx, req)
}

func TestAuthAttemptSharesDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		procedure string
		run       func(*testing.T, *testcp.Server, *Client, string, *identity.Key)
	}{
		{
			name:      "Authenticate",
			procedure: ownsandv1connect.AgentServiceAuthenticateProcedure,
			run: func(t *testing.T, _ *testcp.Server, c *Client, id string, key *identity.Key) {
				res, err := c.Authenticate(context.Background(), id, key, "")
				if err != nil {
					t.Fatalf("Authenticate: %v", err)
				}
				if res.Session == nil || res.Session.Credential == "" {
					t.Fatalf("Authenticate returned no session: %+v", res)
				}
			},
		},
		{
			name:      "ConfirmRemoval",
			procedure: ownsandv1connect.AgentServiceConfirmRemovalProcedure,
			run: func(t *testing.T, cp *testcp.Server, c *Client, id string, key *identity.Key) {
				cp.MarkRemoved(id)
				if err := c.ConfirmRemoval(context.Background(), id, key); err != nil {
					t.Fatalf("ConfirmRemoval: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, c, id, key := enrolled(t)
			c.AuthTimeout = authTestMaxElapsed
			recorder := &authRPCRecorder{AgentServiceClient: c.Agent}
			c.Agent = recorder

			tc.run(t, cp, c, id, key)

			calls := recorder.calls
			if len(calls) != 2 || calls[0].procedure != ownsandv1connect.AgentServiceCreateChallengeProcedure || calls[1].procedure != tc.procedure {
				t.Fatalf("RPC calls = %+v, want CreateChallenge followed by %s", calls, tc.name)
			}
			for _, call := range calls {
				if !call.hasDeadline {
					t.Fatalf("%s has no deadline", call.procedure)
				}
			}
			if !calls[0].deadline.Equal(calls[1].deadline) {
				t.Fatalf("CreateChallenge deadline = %v, %s deadline = %v, want the same deadline", calls[0].deadline, tc.name, calls[1].deadline)
			}
		})
	}
}

func TestAuthenticateEndsWhenTheControlPlaneStalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall func(*testcp.Server)
	}{
		{"create challenge", (*testcp.Server).StallNextCreateChallenge},
		{"authenticate", func(cp *testcp.Server) { cp.StallNextAuthenticate() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, c, id, key := enrolled(t)
			tc.stall(cp)

			start := time.Now()
			res, err := c.Authenticate(context.Background(), id, key, "")
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("Authenticate of a stalled attempt returned %+v", res)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error %v does not match context.DeadlineExceeded", err)
			}
			if want := "cpclient: " + tc.name + ": no answer within " + authTestTimeout.String(); !strings.Contains(err.Error(), want) {
				t.Fatalf("Authenticate error = %v, want %q", err, want)
			}
			if errors.Is(err, ErrUnknownDevice) {
				t.Fatalf("a stalled attempt was reported as an unknown device: %v", err)
			}
			if elapsed < authTestTimeout-authTestSlack || elapsed > authTestMaxElapsed {
				t.Fatalf("Authenticate returned after %s, want about %s", elapsed, authTestTimeout)
			}

			// The next attempt is normal and is not bound to the expired deadline.
			res, err = c.Authenticate(context.Background(), id, key, "")
			if err != nil {
				t.Fatalf("Authenticate after a stalled attempt: %v", err)
			}
			if res.Session == nil || res.Session.Credential == "" {
				t.Fatalf("Authenticate returned no session: %+v", res)
			}
		})
	}
}

func TestAuthenticateKeepsTheCallerContext(t *testing.T) {
	cp, c, id, key := enrolled(t)
	c.AuthTimeout = time.Hour
	entered := cp.StallNextAuthenticate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Authenticate(ctx, id, key, "")
		done <- err
	}()

	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("Authenticate returned before the handler entered the stall: %v", err)
	case <-time.After(authTestMaxElapsed):
		t.Fatal("Authenticate handler did not enter the stall")
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(authTestMaxElapsed):
		t.Fatal("Authenticate did not return after caller cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not match context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a canceled caller was reported as a deadline: %v", err)
	}
	if strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("a canceled caller was reported as an agent timeout: %v", err)
	}
}

func TestAuthenticateUnknownDeviceIsNotATimeout(t *testing.T) {
	cp, c, id, key := enrolled(t)
	cp.Forget(id)
	_, err := c.Authenticate(context.Background(), id, key, "")
	if !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("error %v does not match ErrUnknownDevice", err)
	}
}

func TestConfirmRemovalEndsWhenTheControlPlaneStalls(t *testing.T) {
	cp, c, id, key := enrolled(t)
	cp.StallNextCreateChallenge()
	start := time.Now()
	err := c.ConfirmRemoval(context.Background(), id, key)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not match context.DeadlineExceeded", err)
	}
	if elapsed < authTestTimeout-authTestSlack || elapsed > authTestMaxElapsed {
		t.Fatalf("ConfirmRemoval returned after %s, want about %s", elapsed, authTestTimeout)
	}
}

func TestEnrollContextDeadline(t *testing.T) {
	c := &Client{}
	if got := c.EnrollTimeout(); got != DefaultAuthTimeout {
		t.Fatalf("zero AuthTimeout gives EnrollTimeout %s, want %s", got, DefaultAuthTimeout)
	}
	c.AuthTimeout = time.Minute
	ctx, cancel := c.EnrollContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
		t.Fatalf("deadline = %v, %v; want about one minute", deadline, ok)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	ctx2, cancel2 := c.EnrollContext(parent)
	defer cancel2()
	if d, _ := ctx2.Deadline(); time.Until(d) > time.Second {
		t.Fatalf("an earlier caller deadline must win, got %v", d)
	}
}
