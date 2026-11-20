package ownsand

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func validCreate() CreateSandboxOptions {
	return CreateSandboxOptions{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX, VCPUCount: 2, MemoryMiB: 2048, DiskGiB: 10,
		IdleTTL: 10 * time.Minute, MaxTTL: time.Hour}
}

// TestCreateValidatesTTLBeforeRequest checks TTL and resource validation
// before any request reaches the server.
func TestCreateValidatesTTLBeforeRequest(t *testing.T) {
	e := newEnv(t, TransportRelay)
	cases := map[string]func(o *CreateSandboxOptions){
		"missing idle TTL":   func(o *CreateSandboxOptions) { o.IdleTTL = 0 },
		"missing max TTL":    func(o *CreateSandboxOptions) { o.MaxTTL = 0 },
		"idle above max":     func(o *CreateSandboxOptions) { o.IdleTTL = 2 * time.Hour },
		"max above 7 days":   func(o *CreateSandboxOptions) { o.MaxTTL = MaxTTL + time.Second; o.IdleTTL = time.Hour },
		"missing resources":  func(o *CreateSandboxOptions) { o.MemoryMiB = 0 },
		"two sources":        func(o *CreateSandboxOptions) { o.SnapshotID = "s" },
		"no source":          func(o *CreateSandboxOptions) { o.BaseImage = 0 },
		"negative idle TTL":  func(o *CreateSandboxOptions) { o.IdleTTL = -time.Second },
		"zero vCPU and disk": func(o *CreateSandboxOptions) { o.VCPUCount, o.DiskGiB = 0, 0 },
	}
	for name, mutate := range cases {
		o := validCreate()
		mutate(&o)
		_, err := e.c.CreateSandbox(e.ctx, o)
		if CodeOf(err) != connect.CodeInvalidArgument || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if n := e.srv.CreateCount(); n != 0 {
		t.Fatalf("server saw %d creates", n)
	}
	o := validCreate()
	o.MaxTTL, o.IdleTTL = MaxTTL, MaxTTL
	if _, err := e.c.CreateSandbox(e.ctx, o); err != nil {
		t.Fatalf("exactly 7 days rejected: %v", err)
	}
}

func TestCreateReusesIdempotencyKeyAfterLostResponse(t *testing.T) {
	e := newEnv(t, TransportRelay)
	var keys []string
	e.srv.OnCreateSandbox = func(r *ownsandv1.CreateSandboxRequest) *ownsandtest.Fault {
		keys = append(keys, r.GetIdempotencyKey())
		if len(keys) == 1 {
			return &ownsandtest.Fault{LoseResponse: true}
		}
		return nil
	}
	sb, err := e.c.CreateSandbox(e.ctx, validCreate())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != keys[1] || e.srv.CreateCount() != 1 || sb.ID() == "" {
		t.Fatalf("keys %v creates %d", keys, e.srv.CreateCount())
	}
	// A second logical create uses a new key.
	if _, err := e.c.CreateSandbox(e.ctx, validCreate()); err != nil || keys[2] == keys[0] {
		t.Fatalf("second create: %v %v", err, keys)
	}
}

func TestOrganizationRequiredBeforeRequest(t *testing.T) {
	t.Setenv(EnvOrganization, "")
	srv := ownsandtest.New(t, ownsandtest.Options{})
	c, err := New(Options{Origin: srv.URL, APIKey: ownsandtest.DefaultAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIceServers(context.Background()); !errors.Is(err, ErrNoOrganization) {
		t.Fatalf("err = %v", err)
	}
	if len(srv.SeenCredentials) != 0 {
		t.Fatal("request sent without an organization")
	}
	if _, err := c.GetIceServers(context.Background(), WithOrganization(srv.Org)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOrganization, srv.Org)
	c2, _ := New(Options{Origin: srv.URL, APIKey: ownsandtest.DefaultAPIKey})
	if c2.OrganizationID() != srv.Org {
		t.Fatal("OWNSAND_ORG not read")
	}
}

func TestCredentialFromEnvironment(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := New(Options{}); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v", err)
	}
	srv := ownsandtest.New(t, ownsandtest.Options{})
	t.Setenv(EnvAPIKey, ownsandtest.DefaultAPIKey)
	c, err := New(Options{Origin: srv.URL, OrganizationID: srv.Org})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIceServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := srv.SeenCredentials[len(srv.SeenCredentials)-1]; got != "Bearer "+ownsandtest.DefaultAPIKey {
		t.Fatal("API key from OWNSAND_API_KEY not sent")
	}
	if c.Origin() != srv.URL {
		t.Fatal("origin")
	}
	d, _ := New(Options{APIKey: "osk_x"})
	if d.Origin() != DefaultOrigin {
		t.Fatalf("default origin %q", d.Origin())
	}
	if _, err := New(Options{APIKey: "osk_x", Origin: "https://example.com/path"}); err == nil {
		t.Fatal("origin with a path accepted")
	}
}

// TestCredentialsOnlyToConfiguredOrigin checks that a redirect never
// carries the credential to another origin and that the transport refuses
// other origins.
func TestCredentialsOnlyToConfiguredOrigin(t *testing.T) {
	var foreignHits atomic.Int32
	var foreignAuth atomic.Value
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		foreignAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c, err := New(Options{Origin: redirector.URL, APIKey: "osk_secret-value", OrganizationID: "org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIceServers(context.Background()); err == nil {
		t.Fatal("redirect followed")
	}
	if foreignHits.Load() != 0 {
		t.Fatalf("foreign origin received %d requests (auth %v)", foreignHits.Load(), foreignAuth.Load())
	}
	req, _ := http.NewRequest(http.MethodGet, foreign.URL+"/x", nil)
	if _, err := c.http.Do(req); !errors.Is(err, errForeignOrigin) {
		t.Fatalf("foreign request: %v", err)
	}
	if foreignHits.Load() != 0 {
		t.Fatal("foreign origin reached")
	}
}

func TestCredentialRedactionInErrors(t *testing.T) {
	e := newEnv(t, TransportRelay)
	e.srv.OnCreateSandbox = func(*ownsandv1.CreateSandboxRequest) *ownsandtest.Fault {
		return &ownsandtest.Fault{Err: connect.NewError(connect.CodeInternal, errors.New("bad key "+ownsandtest.DefaultAPIKey))}
	}
	_, err := e.c.CreateSandbox(e.ctx, validCreate())
	if err == nil || strings.Contains(err.Error(), ownsandtest.DefaultAPIKey) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("err = %v", err)
	}
	if CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v", CodeOf(err))
	}

	// A token function's tokens are redacted too.
	e.srv.AddUser("logto-token-abc", "user-1")
	tc, _ := New(Options{Origin: e.srv.URL, OrganizationID: e.srv.Org, Clock: e.srv.Clock,
		Token: func(context.Context) (string, error) { return "logto-token-abc", nil }})
	e.srv.OnCreateSandbox = func(*ownsandv1.CreateSandboxRequest) *ownsandtest.Fault {
		return &ownsandtest.Fault{Err: connect.NewError(connect.CodeInternal, errors.New("token logto-token-abc rejected"))}
	}
	_, err = tc.CreateSandbox(e.ctx, validCreate())
	if err == nil || strings.Contains(err.Error(), "logto-token-abc") {
		t.Fatalf("err = %v", err)
	}
}
