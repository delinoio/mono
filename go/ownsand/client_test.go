// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestNewReadsEnvironment(t *testing.T) {
	c, err := New(Options{LookupEnv: envOf(map[string]string{EnvAPIKey: "osk_x", EnvOrganization: "org"})})
	if err != nil {
		t.Fatal(err)
	}
	if !c.UsesAPIKey() || c.OrganizationID() != "org" || c.BaseURL() != DefaultBaseURL {
		t.Fatalf("unexpected client: apiKey=%v org=%q base=%q", c.UsesAPIKey(), c.OrganizationID(), c.BaseURL())
	}
	if _, err := New(Options{LookupEnv: envOf(nil)}); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
	if _, err := New(Options{APIKey: "osk_x", TokenSource: func(context.Context) (string, error) { return "", nil }}); err == nil {
		t.Fatal("want an error for two credentials")
	}
	for _, bad := range []string{"ftp://x", "https://x/path", "x", "https://x#f", "https://x#", "https://x?", "https://x/?", "https://x?a=b", "https://u:p@x"} {
		if _, err := New(Options{BaseURL: bad, APIKey: "osk_x"}); err == nil {
			t.Fatalf("base URL %q accepted", bad)
		} else if strings.Contains(err.Error(), bad) {
			t.Fatalf("error echoes the base URL: %v", err)
		}
	}
}

func TestOrganizationRequiredBeforeSend(t *testing.T) {
	cp := newFakeCP(t)
	c, err := New(Options{BaseURL: cp.srv.URL, APIKey: testAPIKey, LookupEnv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Organizations().Get(context.Background(), nil); !errors.Is(err, ErrNoOrganization) {
		t.Fatalf("want ErrNoOrganization, got %v", err)
	}
	if _, err := c.Sandbox("", "x"); !errors.Is(err, ErrNoOrganization) {
		t.Fatalf("want ErrNoOrganization, got %v", err)
	}
	cp.mu.Lock()
	n := len(cp.auths)
	cp.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests sent without an organization", n)
	}
	// An explicit organization overrides the missing default, and RPCs
	// without an organization work.
	res, err := c.Organizations().Get(context.Background(), &ownsandv1.GetOrganizationRequest{OrganizationId: cp.orgID})
	if err != nil || res.GetOrganization().GetOrganizationId() != cp.orgID {
		t.Fatalf("Get: %v %v", res, err)
	}
	if _, err := c.Organizations().List(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestTokenSource(t *testing.T) {
	cp := newFakeCP(t)
	calls := 0
	c, err := New(Options{BaseURL: cp.srv.URL, OrganizationID: cp.orgID, TokenSource: func(context.Context) (string, error) {
		calls++
		return testAPIKey, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Organizations().Get(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("token source called %d times", calls)
	}
}

func TestCredentialsOnlyToConfiguredOrigin(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.redirectTo = other.URL
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	if _, err := c.Organizations().Get(context.Background(), nil); err == nil {
		t.Fatal("redirected request succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the redirect target received %d requests", len(leaked))
	}
}

func TestCredentialRedactedInErrors(t *testing.T) {
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.echoToken = true
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	_, err := c.Organizations().Get(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("error contains the credential: %v", err)
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code %v", connect.CodeOf(err))
	}
	// Streams too.
	s := cp.handle(t, c)
	_, err = s.Exec(context.Background(), ExecParams{Command: []string{"echo"}})
	if err == nil || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("exec error: %v", err)
	}
}

func validParams() CreateSandboxParams {
	return CreateSandboxParams{
		BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX,
		Resources: Resources{VCPUCount: 2, MemoryMiB: 2048, DiskGiB: 10},
		IdleTTL:   10 * time.Minute,
		MaxTTL:    time.Hour,
	}
}

func TestCreateSandboxValidatesBeforeRequest(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	cases := map[string]func(*CreateSandboxParams){
		"missing idle ttl": func(p *CreateSandboxParams) { p.IdleTTL = 0 },
		"missing max ttl":  func(p *CreateSandboxParams) { p.MaxTTL = 0 },
		"idle above max":   func(p *CreateSandboxParams) { p.IdleTTL = 2 * time.Hour },
		"max above 7 days": func(p *CreateSandboxParams) { p.MaxTTL = 8 * 24 * time.Hour },
		"missing vcpu":     func(p *CreateSandboxParams) { p.Resources.VCPUCount = 0 },
		"missing memory":   func(p *CreateSandboxParams) { p.Resources.MemoryMiB = 0 },
		"missing disk":     func(p *CreateSandboxParams) { p.Resources.DiskGiB = 0 },
		"two sources":      func(p *CreateSandboxParams) { p.TemplateID = mustID(t) },
		"no source":        func(p *CreateSandboxParams) { p.BaseImage = 0 },
	}
	for name, mutate := range cases {
		p := validParams()
		mutate(&p)
		if _, err := c.CreateSandbox(context.Background(), p); !validate.IsValidationError(err) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.createCalls != 0 {
		t.Fatalf("%d create requests sent", cp.createCalls)
	}
}

func TestCreateSandboxRetriesLostResponseWithSameKey(t *testing.T) {
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.dropCreates = 2
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	sb, err := c.CreateSandbox(context.Background(), validParams())
	if err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	calls, keys := cp.createCalls, len(cp.created)
	cp.mu.Unlock()
	if calls != 3 || keys != 1 {
		t.Fatalf("calls=%d distinct keys=%d, want 3 and 1", calls, keys)
	}
	if sb.Info().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_CREATING {
		t.Fatalf("state %v", sb.Info().GetState())
	}
	got, err := sb.WaitStable(context.Background())
	if err != nil || got.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable: %v %v", got, err)
	}
}

func TestCreateSandboxStalledRetryEndsWithWindow(t *testing.T) {
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.dropCreates = 1
	cp.stallCreates = 1
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	window := c.timing.createRetryWindow
	type result struct {
		sb  *Sandbox
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	// A caller context without a deadline: only the retry window can end
	// the stalled retry.
	go func() {
		sb, err := c.CreateSandbox(context.Background(), validParams())
		done <- result{sb, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(window + 5*time.Second):
		t.Fatal("CreateSandbox did not end with the retry window")
	}
	elapsed := time.Since(start)
	if r.err == nil || r.sb != nil {
		t.Fatalf("CreateSandbox returned %v, %v; want the last retryable error", r.sb, r.err)
	}
	if kind, _ := classify(context.Background(), r.err); kind != failInterrupted {
		t.Fatalf("error %v is %v, want the last transport interruption", r.err, kind)
	}
	if errors.Is(r.err, context.Canceled) || errors.Is(r.err, errCreateDeadline) {
		t.Fatalf("error %v is the SDK cancellation, want the last failure of the window", r.err)
	}
	if elapsed < window*8/10 || elapsed > window+2*time.Second {
		t.Fatalf("CreateSandbox ended after %v, want about %v", elapsed, window)
	}
	cp.mu.Lock()
	calls, keys := cp.createCalls, append([]string(nil), cp.createKeys...)
	cp.mu.Unlock()
	if calls != 2 || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("calls=%d keys=%q, want 2 attempts with one idempotency key", calls, keys)
	}
}

// awaitCreateCalls polls the fake control plane until it has seen at least n
// CreateSandbox calls. It reports false when ctx ends first, so a call that
// never arrives cannot keep the poll loop running.
func awaitCreateCalls(ctx context.Context, cp *fakeCP, n int) bool {
	for {
		cp.mu.Lock()
		calls := cp.createCalls
		cp.mu.Unlock()
		if calls >= n {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAwaitCreateCallsStopsWhenTheCallNeverArrives(t *testing.T) {
	cp := newFakeCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- awaitCreateCalls(ctx, cp, 1) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("awaitCreateCalls reported a CreateSandbox call that never happened")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitCreateCalls kept polling after its context ended")
	}
}

func TestCreateSandboxCancelDuringStalledRetryIsTerminal(t *testing.T) {
	cp := newFakeCP(t)
	cp.mu.Lock()
	cp.dropCreates = 1
	cp.stallCreates = 1
	cp.mu.Unlock()
	c := cp.client(t, TransportRelay)
	// The test-local deadline makes a retry that never arrives fail fast, and
	// ends the poll goroutine, instead of waiting for the retry window.
	base, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	var poll sync.WaitGroup
	poll.Add(1)
	t.Cleanup(poll.Wait)
	retried := make(chan bool, 1)
	go func() {
		defer poll.Done()
		// Cancel once the stalled retry is in flight.
		if !awaitCreateCalls(ctx, cp, 2) {
			retried <- false
			return
		}
		retried <- true
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.CreateSandbox(ctx, validParams())
	cancel()
	if !<-retried {
		t.Fatalf("the stalled retry never arrived; CreateSandbox returned %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want the caller's context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed >= c.timing.createRetryWindow {
		t.Fatalf("cancellation took %v, want before the retry window", elapsed)
	}
}

func TestSandboxLifecycle(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	ctx := context.Background()
	if sb, err := s.Pause(ctx); err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause: %v %v", sb, err)
	}
	if sb, err := s.Resume(ctx); err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume: %v %v", sb, err)
	}
	if sb, err := s.Delete(ctx); err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("delete: %v %v", sb, err)
	}
}

func TestParseTransport(t *testing.T) {
	for _, tr := range []Transport{TransportAuto, TransportPeerToPeer, TransportRelay} {
		got, err := ParseTransport(tr.String())
		if err != nil || got != tr {
			t.Fatalf("%v: %v %v", tr, got, err)
		}
	}
	if _, err := ParseTransport("x"); err == nil {
		t.Fatal("want an error")
	}
}

func TestRedactKeepsWireProvenance(t *testing.T) {
	c := &Client{}
	detail, err := connect.NewErrorDetail(&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE})
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []bool{true, false} {
		raw := errors.New("bad credential " + testAPIKey)
		var in *connect.Error
		if wire {
			in = connect.NewWireError(connect.CodeUnauthenticated, raw)
		} else {
			in = connect.NewError(connect.CodeUnauthenticated, raw)
		}
		in.AddDetail(detail)
		in.Meta().Set("X-Test", "1")
		out := c.redact(in, testAPIKey)
		if strings.Contains(out.Error(), testAPIKey) {
			t.Fatalf("wire=%v: error contains the credential: %v", wire, out)
		}
		var ce *connect.Error
		if !errors.As(out, &ce) {
			t.Fatalf("wire=%v: not a connect error", wire)
		}
		if connect.IsWireError(out) != wire {
			t.Fatalf("wire=%v: IsWireError=%v", wire, connect.IsWireError(out))
		}
		if ce.Code() != connect.CodeUnauthenticated || len(ce.Details()) != 1 || ce.Meta().Get("X-Test") != "1" {
			t.Fatalf("wire=%v: code, details or meta lost: %v", wire, ce)
		}
		wantKind, _ := classify(context.Background(), in)
		if gotKind, _ := classify(context.Background(), out); gotKind != wantKind {
			t.Fatalf("wire=%v: classify %v, want %v", wire, gotKind, wantKind)
		}
	}
	if kind, _ := classify(context.Background(), c.redact(connect.NewWireError(connect.CodeUnauthenticated, errors.New(testAPIKey)), testAPIKey)); kind != failTerminal {
		t.Fatalf("redacted terminal wire error classified %v", kind)
	}
}

func TestOrganizationDefaultKeepsCallerRequestUnchanged(t *testing.T) {
	cp := newFakeCP(t)
	newClient := func(org string) *Client {
		c, err := New(Options{BaseURL: cp.srv.URL, APIKey: testAPIKey, OrganizationID: org, LookupEnv: func(string) (string, bool) { return "", false }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := newClient("org-first"), newClient("org-second")
	req := &ownsandv1.GetOrganizationRequest{}
	for _, tc := range []struct {
		c    *Client
		want string
	}{{first, "org-first"}, {second, "org-second"}} {
		res, err := tc.c.Organizations().Get(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.GetOrganization().GetOrganizationId(); got != tc.want {
			t.Fatalf("RPC saw organization %q, want %q", got, tc.want)
		}
		if req.GetOrganizationId() != "" {
			t.Fatalf("caller request was modified: organization_id=%q", req.GetOrganizationId())
		}
	}
	// An explicit organization wins and stays as the caller set it.
	explicit := &ownsandv1.GetOrganizationRequest{OrganizationId: "org-explicit"}
	res, err := first.Organizations().Get(context.Background(), explicit)
	if err != nil || res.GetOrganization().GetOrganizationId() != "org-explicit" {
		t.Fatalf("explicit organization: %v %v", res, err)
	}
}

// upperUUID returns id with every hex letter in uppercase.
func upperUUID(id string) string { return strings.ToUpper(id) }

// mixedUUID returns id with the hex letters at odd positions in uppercase.
func mixedUUID(id string) string {
	b := []byte(id)
	for i, c := range b {
		if i%2 == 1 && c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func TestSandboxHandleStoresCanonicalLowercaseID(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	for name, spelling := range map[string]string{
		"lowercase": cp.sandboxID,
		"uppercase": upperUUID(cp.sandboxID),
		"mixed":     mixedUUID(cp.sandboxID),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := c.Sandbox("", spelling)
			if err != nil {
				t.Fatal(err)
			}
			if s.ID() != cp.sandboxID || s.Info().GetSandboxId() != cp.sandboxID {
				t.Fatalf("handle ID %q, info ID %q, want %q", s.ID(), s.Info().GetSandboxId(), cp.sandboxID)
			}
		})
	}
}

func TestSandboxHandleKeepsInvalidID(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	for _, bad := range []string{"x", "", upperUUID(cp.sandboxID) + "0", "0190A5B2-0000-4000-8000-000000000000"} {
		s, err := c.Sandbox("", bad)
		if err != nil {
			t.Fatal(err)
		}
		if s.ID() != bad {
			t.Fatalf("invalid ID %q stored as %q", bad, s.ID())
		}
		_, err = s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
		if !validate.IsValidationError(err) {
			t.Fatalf("invalid ID %q: want a validation error, got %v", bad, err)
		}
	}
}

// TestSandboxHandleUppercaseIDUsesCanonicalSpellingOnEveryPath opens a handle
// with a non-canonical spelling. The fake control plane and agent compare the
// identifier as text after they canonicalize it, as the real ones do, so a
// handle that sent the given spelling would fail the peer-to-peer frame check
// or the relay digest check.
func TestSandboxHandleUppercaseIDUsesCanonicalSpellingOnEveryPath(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			c := cp.client(t, tr)
			s, err := c.Sandbox("", mixedUUID(upperUUID(cp.sandboxID)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if _, err := s.Get(context.Background()); err != nil {
				t.Fatalf("get: %v", err)
			}
			res, r := runExec(t, s)
			checkLines(t, res, r, 3)
			cp.agent.setFile("/tmp/a", []byte("hello"))
			got, err := s.ReadFileBytes(context.Background(), "/tmp/a")
			if err != nil || string(got) != "hello" {
				t.Fatalf("read: %q, %v", got, err)
			}
			if err := s.WriteFileBytes(context.Background(), "/tmp/b", []byte("world"), WriteOptions{Mode: 0o600}); err != nil {
				t.Fatalf("write: %v", err)
			}
			if b, ok := cp.agent.file("/tmp/b"); !ok || string(b) != "world" {
				t.Fatalf("written file %q, %v", b, ok)
			}
		})
	}
}
