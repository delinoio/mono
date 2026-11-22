// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestCreateOrganizationNameCharacters checks the display name rules over
// RPC: the replacement character U+FFFD is ordinary text, and a C1 control
// character fails with invalid_argument.
func TestCreateOrganizationNameCharacters(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	alice := env.User("alice")
	create := func(name string) error {
		_, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: name}))
		return err
	}
	if err := create("A�Z"); err != nil {
		t.Fatalf("name with U+FFFD: %v", err)
	}
	for _, name := range []string{"A\u0080Z", "A\u0085Z", "A\u009fZ", "A\x7fZ", "A\x1fZ"} {
		if err := create(name); testenv.Code(err) != connect.CodeInvalidArgument {
			t.Fatalf("name %q: code = %v (%v), want invalid_argument", name, testenv.Code(err), err)
		}
	}
	if n := countRows(t, env, "SELECT count(*) FROM memberships WHERE user_id = $1", "alice"); n != 1 {
		t.Fatalf("alice memberships = %d, want 1 (only the valid name created an organization)", n)
	}
}
