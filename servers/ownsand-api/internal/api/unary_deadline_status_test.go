// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestListOrganizationsContextStatus checks that a unary handler whose
// database query fails because the request context ended reports the
// matching status: deadline_exceeded for a deadline that already passed and
// canceled for a canceled caller.
func TestListOrganizationsContextStatus(t *testing.T) {
	env := testenv.New(t)
	handler := api.OrganizationHandler{Service: env.App.Service}
	principal := auth.WithPrincipal(context.Background(), auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: "alice"})

	expired, cancelExpired := context.WithDeadline(principal, time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(principal)
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		want connect.Code
	}{
		{"expired deadline", expired, connect.CodeDeadlineExceeded},
		{"canceled caller", canceled, connect.CodeCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := handler.ListOrganizations(tc.ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{}))
			if got := testenv.Code(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}
