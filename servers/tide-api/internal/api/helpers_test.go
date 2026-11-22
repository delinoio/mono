// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

// fakeDevSessions stands in for the later dev session layer: it is mounted
// through the Registry like a real layer and authorizes with
// ActionUseDevSessions, so the tests can check that personal API keys work
// there and nowhere else.
type fakeDevSessions struct {
	tidev1connect.UnimplementedDevSessionServiceHandler
	svc *api.Service
}

func (f *fakeDevSessions) ListDevSessionProfiles(ctx context.Context, req *connect.Request[tidev1.ListDevSessionProfilesRequest]) (*connect.Response[tidev1.ListDevSessionProfilesResponse], error) {
	if _, err := f.svc.Authorize(ctx, f.svc.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions); err != nil {
		return nil, err
	}
	return connect.NewResponse(&tidev1.ListDevSessionProfilesResponse{}), nil
}

func withFakeDevSessions() testenv.Option {
	return testenv.WithExtension(func(r *app.Registry) error {
		r.Handle(tidev1connect.NewDevSessionServiceHandler(&fakeDevSessions{svc: r.Service}, r.HandlerOptions()...))
		return nil
	})
}

func listProfiles(c *testenv.Clients, orgID string) error {
	_, err := c.DevSession.ListDevSessionProfiles(context.Background(), connect.NewRequest(&tidev1.ListDevSessionProfilesRequest{OrganizationId: orgID}))
	return err
}

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if got := testenv.Code(err); got != want {
		t.Fatalf("%s: code %v (%v), want %v", what, got, err, want)
	}
}

// auditActions returns the actions of the organization's audit events,
// newest first.
func auditActions(t *testing.T, env *testenv.Env, orgID string) []tidev1.AuditAction {
	t.Helper()
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT action FROM audit_events WHERE organization_id = $1 ORDER BY audit_event_id DESC", orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []tidev1.AuditAction
	for rows.Next() {
		var a int16
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, tidev1.AuditAction(a))
	}
	return out
}

func countAction(actions []tidev1.AuditAction, a tidev1.AuditAction) int {
	n := 0
	for _, x := range actions {
		if x == a {
			n++
		}
	}
	return n
}
