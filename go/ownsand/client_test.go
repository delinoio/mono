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
	for _, bad := range []string{"ftp://x", "https://x/path", "x"} {
		if _, err := New(Options{BaseURL: bad, APIKey: "osk_x"}); err == nil {
			t.Fatalf("base URL %q accepted", bad)
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
