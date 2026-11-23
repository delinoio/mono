// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// The credential marker lets the tests check that no error message contains
// the credential. The newline makes the value invalid as a header value.
const (
	badCredentialMarker = "osk_bad-credential-marker"
	badCredential       = badCredentialMarker + "\nsecond-line"
)

// safetyTimeout bounds each operation of these tests, so that a regression
// that retries the invalid credential fails the test instead of hanging it.
// A passing run never waits for it.
const safetyTimeout = 10 * time.Second

// forbiddenTransport records every request that the SDK sends and fails it.
// The invalid-credential tests use it as the HTTP transport, so that any
// request (also one that Go's transport would reject itself) is visible.
type forbiddenTransport struct {
	mu    sync.Mutex
	calls int
}

func (f *forbiddenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil, errors.New("the transport must not be called")
}

func (f *forbiddenTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// countingTokenSource returns the values of its script, one per call, and the
// last value for every later call.
type countingTokenSource struct {
	mu     sync.Mutex
	calls  int
	script []string
}

func (ts *countingTokenSource) fn(context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := min(ts.calls, len(ts.script)-1)
	ts.calls++
	return ts.script[i], nil
}

func (ts *countingTokenSource) callCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls
}

func requireInvalidCredentialError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the operation succeeded with an invalid credential")
	}
	if !errors.Is(err, errInvalidCredential) {
		t.Fatalf("the error is not the invalid credential error (code %v)", connect.CodeOf(err))
	}
	var lc *errLocalCredential
	if !errors.As(err, &lc) {
		t.Fatal("the error is not a local credential error")
	}
	if strings.Contains(err.Error(), badCredentialMarker) || strings.Contains(err.Error(), "second-line") {
		t.Fatal("the error message contains the credential")
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		t.Fatalf("the error is a Connect error with code %v: it must not look like a server or transport status", ce.Code())
	}
}

func TestInvalidHeaderCredentialFailsBeforeTransport(t *testing.T) {
	cases := []struct {
		name string
		// build returns the client options and a function that reports the
		// number of token source calls (0 for a fixed credential).
		build func() (Options, func() int)
	}{
		{name: "fixed API key", build: func() (Options, func() int) {
			return Options{APIKey: badCredential}, func() int { return 0 }
		}},
		{name: "environment API key", build: func() (Options, func() int) {
			return Options{LookupEnv: func(k string) (string, bool) {
				if k == EnvAPIKey {
					return badCredential, true
				}
				return "", false
			}}, func() int { return 0 }
		}},
		{name: "token source", build: func() (Options, func() int) {
			ts := &countingTokenSource{script: []string{badCredential}}
			return Options{TokenSource: ts.fn}, ts.callCount
		}},
		{name: "carriage return", build: func() (Options, func() int) {
			ts := &countingTokenSource{script: []string{"tok\rbad"}}
			return Options{TokenSource: ts.fn}, ts.callCount
		}},
		{name: "NUL byte", build: func() (Options, func() int) {
			ts := &countingTokenSource{script: []string{"tok\x00bad"}}
			return Options{TokenSource: ts.fn}, ts.callCount
		}},
		{name: "DEL byte", build: func() (Options, func() int) {
			ts := &countingTokenSource{script: []string{"tok\x7fbad"}}
			return Options{TokenSource: ts.fn}, ts.callCount
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &forbiddenTransport{}
			opts, tokenCalls := tc.build()
			opts.BaseURL = "https://127.0.0.1:1"
			opts.OrganizationID = mustID(t)
			opts.HTTPClient = &http.Client{Transport: rt}
			c, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}

			// CreateSandbox retries a transport interruption for the whole
			// retry window, so a regression would show as a transport call.
			ctx, cancel := context.WithTimeout(context.Background(), safetyTimeout)
			defer cancel()
			sb, err := c.CreateSandbox(ctx, validParams())
			if sb != nil {
				t.Fatal("CreateSandbox returned a sandbox")
			}
			requireInvalidCredentialError(t, err)
			if n := rt.callCount(); n != 0 {
				t.Fatalf("%d transport calls, want 0", n)
			}
			if n := tokenCalls(); n > 1 {
				t.Fatalf("%d token source calls, want at most 1", n)
			}
		})
	}
}

func TestCheckBearerAcceptsOnlyValidHeaderValues(t *testing.T) {
	for _, tok := range []string{"osk_test-secret-key-value", "a b", " tab\there ", "café", "eyJhbGciOi.eyJzdWIi.c2ln"} {
		if err := checkBearer(tok); err != nil {
			t.Errorf("a valid credential was rejected: %v", err)
		}
	}
	for _, tok := range []string{"a\nb", "a\rb", "a\x00b", "a\x7fb", "a\x1bb", badCredential} {
		if err := checkBearer(tok); !errors.Is(err, errInvalidCredential) {
			t.Errorf("an invalid credential was accepted: %v", err)
		}
	}
}

// stubStreamConn is the connection that the next function of the streaming
// interceptor returns; the interceptor only reads its request header.
type stubStreamConn struct {
	connect.StreamingClientConn
	header http.Header
}

func (s *stubStreamConn) RequestHeader() http.Header { return s.header }

func TestInvalidHeaderCredentialFailsStreamingInterceptor(t *testing.T) {
	rt := &forbiddenTransport{}
	ts := &countingTokenSource{script: []string{badCredential}}
	c, err := New(Options{BaseURL: "https://127.0.0.1:1", TokenSource: ts.fn, OrganizationID: mustID(t), HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	inner := &stubStreamConn{header: http.Header{}}
	next := func(context.Context, connect.Spec) connect.StreamingClientConn { return inner }
	conn := (&authInterceptor{client: c}).WrapStreamingClient(next)(context.Background(), connect.Spec{})

	requireInvalidCredentialError(t, conn.Send(nil))
	requireInvalidCredentialError(t, conn.Receive(nil))
	if got := inner.header.Get("Authorization"); got != "" {
		t.Fatalf("the invalid credential was set as a header: %q", got)
	}
	if n := rt.callCount(); n != 0 {
		t.Fatalf("%d transport calls, want 0", n)
	}
}

func TestInvalidHeaderCredentialEndsRelayAuthorization(t *testing.T) {
	cp := newFakeCP(t)
	ts := &countingTokenSource{script: []string{badCredential}}
	c, err := New(Options{BaseURL: cp.srv.URL, TokenSource: ts.fn, OrganizationID: cp.orgID, Transport: TransportRelay})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	s := cp.handle(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), safetyTimeout)
	defer cancel()

	_, err = localCredExecOnce(s, ctx)
	requireInvalidCredentialError(t, err)
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls", n)
	}
	cp.mu.Lock()
	requests := len(cp.auths)
	cp.mu.Unlock()
	if requests != 0 {
		t.Fatalf("%d requests reached the control plane", requests)
	}
	if n := ts.callCount(); n != 1 {
		t.Fatalf("%d token source calls, want 1", n)
	}
}

func TestInvalidHeaderCredentialKeepsAutomaticHandleEligibleForPeerSetup(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = func(_ []string, e *fakeExec) { e.finish(exitedEvent(3)) }
	// The first call (GetIceServers) is invalid; every later call is valid.
	ts := &countingTokenSource{script: []string{badCredential, testAPIKey}}
	c, err := New(Options{BaseURL: cp.srv.URL, TokenSource: ts.fn, OrganizationID: cp.orgID, Transport: TransportAuto})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	c.p2p.settingEngine = loopbackSettingEngine
	s := cp.handle(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), safetyTimeout)
	defer cancel()
	before := s.Transport()

	_, err = localCredExecOnce(s, ctx)
	requireInvalidCredentialError(t, err)
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls after an invalid credential", n)
	}
	cp.mu.Lock()
	requests := len(cp.auths)
	cp.mu.Unlock()
	if requests != 0 {
		t.Fatalf("%d requests reached the control plane", requests)
	}
	if got := s.Transport(); got != before {
		t.Fatalf("transport changed from %v to %v", before, got)
	}
	s.dp.mu.Lock()
	relay := s.dp.relay
	s.dp.mu.Unlock()
	if relay {
		t.Fatal("the handle moved to the relay")
	}

	// A later valid token sets the peer session up on the same handle.
	res, err := localCredExecOnce(s, ctx)
	if err != nil {
		t.Fatalf("exec with a valid token: %v", err)
	}
	if res.Transport != p2pTransport || s.Transport() != p2pTransport {
		t.Fatalf("transport after the valid token: result %v, handle %v", res.Transport, s.Transport())
	}
	if n := len(cp.authRequests()); n != 0 {
		t.Fatalf("%d AuthorizeRelay calls after the valid token", n)
	}
}

func TestValidCredentialStillRetriesAfterTransportInterruption(t *testing.T) {
	cp := newFakeCP(t)
	// The control plane drops the response of the first CreateSandbox.
	cp.dropCreates = 1
	ts := &countingTokenSource{script: []string{testAPIKey}}
	c, err := New(Options{BaseURL: cp.srv.URL, TokenSource: ts.fn, OrganizationID: cp.orgID})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()

	ctx, cancel := context.WithTimeout(context.Background(), safetyTimeout)
	defer cancel()
	sb, err := c.CreateSandbox(ctx, validParams())
	if err != nil || sb == nil {
		t.Fatalf("CreateSandbox after an interruption: %v", err)
	}
	cp.mu.Lock()
	calls := cp.createCalls
	cp.mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d CreateSandbox requests, want 2 (the interrupted one and its retry)", calls)
	}
}
