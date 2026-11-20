// SPDX-License-Identifier: FSL-1.1-ALv2

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/pgtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/server"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// fakeTokens accepts "user.<sub>.token" as a Logto access token of <sub>.
type fakeTokens struct{}

func (fakeTokens) Validate(_ context.Context, token string) (auth.UserClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "user" || parts[2] != "token" {
		return auth.UserClaims{}, errors.New("bad test token")
	}
	return auth.UserClaims{Subject: parts[1]}, nil
}

func userToken(sub string) string { return "user." + sub + ".token" }

// syncBuffer collects log output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t      *testing.T
	app    *App
	store  *store.Store
	url    string
	client *http.Client
	logs   *syncBuffer
}

// newHarness starts the app on an h2c httptest server backed by a fresh
// Postgres database.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st := pgtest.New(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.Config{Addr: "127.0.0.1:0", APIResource: config.DefaultAPIResource, CORSAllowedOrigins: []string{"http://127.0.0.1:3300"}}
	a := New(cfg, st, logger, Options{Tokens: fakeTokens{}})
	srv := httptest.NewUnstartedServer(a.Handler)
	srv.Config = server.New(a.Handler, logger)
	srv.Start()
	t.Cleanup(srv.Close)
	return &harness{t: t, app: a, store: st, url: srv.URL, client: h2cClient(), logs: logs}
}

// h2cClient speaks HTTP/2 without TLS, like the Caddy route to the container.
func h2cClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
}

// bearer returns a client option that sets the credential.
func bearer(credential string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if credential != "" {
				req.Header().Set("Authorization", "Bearer "+credential)
			}
			return next(ctx, req)
		}
	}))
}

// fixture is an organization with an owner, an admin, a member, an API key,
// a device with a session credential, an enrollment token and an invitation,
// plus an API key of another organization.
type fixture struct {
	org, otherOrg                 string
	ownerSub, adminSub, memberSub string
	apiKey, otherAPIKey           string
	apiKeyID                      string
	deviceID, deviceCredential    string
	tokenID, invitationID         string
}

func (h *harness) newFixture() fixture {
	h.t.Helper()
	ctx := context.Background()
	st := h.store
	f := fixture{ownerSub: "owner-" + store.NewID(), adminSub: "admin-" + store.NewID(), memberSub: "member-" + store.NewID()}
	org, err := st.CreateOrganization(ctx, "org", f.ownerSub, "")
	must(h.t, err)
	f.org = org.ID
	owner := store.Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: f.ownerSub}
	for sub, role := range map[string]ownsandv1.Role{f.adminSub: ownsandv1.Role_ROLE_ADMIN, f.memberSub: ownsandv1.Role_ROLE_MEMBER} {
		v := secret.New(secret.KindInvitation)
		_, err := st.CreateInvitation(ctx, f.org, role, owner, secret.Hash(v), secret.DisplayPrefix(v), time.Hour, "")
		must(h.t, err)
		_, _, err = st.AcceptInvitation(ctx, secret.Hash(v), sub, "")
		must(h.t, err)
	}
	f.apiKey = secret.New(secret.KindAPIKey)
	k, err := st.CreateAPIKey(ctx, f.org, "key", owner, secret.Hash(f.apiKey), secret.DisplayPrefix(f.apiKey), "")
	must(h.t, err)
	f.apiKeyID = k.ID
	tokenValue := secret.New(secret.KindEnrollmentToken)
	_, err = st.CreateEnrollmentToken(ctx, f.org, "dev", time.Hour, owner, secret.Hash(tokenValue), secret.DisplayPrefix(tokenValue), "")
	must(h.t, err)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	f.deviceID, _, err = st.Enroll(ctx, secret.Hash(tokenValue), pub, &ownsandv1.DeviceCapabilities{}, "")
	must(h.t, err)
	f.deviceCredential = secret.New(secret.KindDeviceSession)
	must(h.t, st.CreateDeviceSession(ctx, f.deviceID, secret.Hash(f.deviceCredential), time.Now().Add(15*time.Minute), "epoch"))
	tok2 := secret.New(secret.KindEnrollmentToken)
	t2, err := st.CreateEnrollmentToken(ctx, f.org, "dev2", time.Hour, owner, secret.Hash(tok2), secret.DisplayPrefix(tok2), "")
	must(h.t, err)
	f.tokenID = t2.ID
	inv := secret.New(secret.KindInvitation)
	i, err := st.CreateInvitation(ctx, f.org, ownsandv1.Role_ROLE_MEMBER, owner, secret.Hash(inv), secret.DisplayPrefix(inv), time.Hour, "")
	must(h.t, err)
	f.invitationID = i.ID
	other, err := st.CreateOrganization(ctx, "other", "other-owner", "")
	must(h.t, err)
	f.otherOrg = other.ID
	f.otherAPIKey = secret.New(secret.KindAPIKey)
	_, err = st.CreateAPIKey(ctx, other.ID, "k", store.Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: "other-owner"}, secret.Hash(f.otherAPIKey), secret.DisplayPrefix(f.otherAPIKey), "")
	must(h.t, err)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
