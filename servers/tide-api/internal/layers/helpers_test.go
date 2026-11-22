// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

type world struct {
	env  *testenv.Env
	fake *ownsandfake.Fake
	l    *layers.Layers
	org  testenv.Org
}

func newWorld(t *testing.T, opts ...testenv.Option) *world {
	t.Helper()
	w := &world{fake: ownsandfake.New(), l: &layers.Layers{}}
	w.env = testenv.New(t, append([]testenv.Option{testenv.WithLayers(w.fake, w.l)}, opts...)...)
	w.org = w.env.NewOrg("alice")
	return w
}

func ownsandKey(c byte) string { return "osk_" + strings.Repeat(string(c), 43) }

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if got := testenv.Code(err); got != want {
		t.Fatalf("%s: code %v (%v), want %v", what, got, err, want)
	}
}

func (w *world) setLink(user, ownsandOrg, key string) (*tidev1.OwnSandLink, error) {
	resp, err := w.env.User(user).OwnSandLink.SetOwnSandLink(context.Background(), connect.NewRequest(&tidev1.SetOwnSandLinkRequest{
		OrganizationId: w.org.ID, OwnsandOrganizationId: ownsandOrg, OwnsandApiKey: key}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetLink(), nil
}

func countAudit(t *testing.T, env *testenv.Env, orgID string, action tidev1.AuditAction) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", orgID, int16(action)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
