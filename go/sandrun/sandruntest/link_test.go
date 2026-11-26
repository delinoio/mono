// SPDX-License-Identifier: Apache-2.0

package sandruntest_test

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestGetOwnSandLink(t *testing.T) {
	server := sandruntest.New()
	t.Cleanup(server.Close)
	orgID := server.AddOrganization("test").GetOrganizationId()
	client, err := sandrun.New(sandrun.Options{BaseURL: server.URL, APIKey: server.AccessToken})
	if err != nil {
		t.Fatal(err)
	}
	links := client.OwnSandLinks()
	get := func(id string) (*connect.Response[sandrunv1.GetOwnSandLinkResponse], error) {
		return links.GetOwnSandLink(t.Context(), connect.NewRequest(&sandrunv1.GetOwnSandLinkRequest{
			OrganizationId: id,
		}))
	}
	checkUnlinked := func(stage string) {
		t.Helper()
		res, err := get(orgID)
		if err != nil {
			t.Fatalf("get link %s: %v", stage, err)
		}
		if link := res.Msg.GetLink(); link != nil {
			t.Fatalf("get link %s = %v, want unset", stage, link)
		}
	}

	checkUnlinked("before set")

	const ownsandOrgID = "018f0000-0000-7000-8000-0000000000a1"
	set, err := links.SetOwnSandLink(t.Context(), connect.NewRequest(&sandrunv1.SetOwnSandLinkRequest{
		OrganizationId:        orgID,
		OwnsandOrganizationId: ownsandOrgID,
		OwnsandApiKey:         "fixture-key",
	}))
	if err != nil {
		t.Fatalf("set link: %v", err)
	}
	linked, err := get(orgID)
	if err != nil {
		t.Fatalf("get linked organization: %v", err)
	}
	if got, want := linked.Msg.GetLink(), set.Msg.GetLink(); got.GetOrganizationId() != orgID ||
		got.GetOwnsandOrganizationId() != ownsandOrgID || !proto.Equal(got, want) {
		t.Fatalf("get stored link = %v, want the link set for %s to %s: %v", got, orgID, ownsandOrgID, want)
	}

	if _, err := links.RemoveOwnSandLink(t.Context(), connect.NewRequest(&sandrunv1.RemoveOwnSandLinkRequest{
		OrganizationId: orgID,
	})); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	checkUnlinked("after removal")

	const unknownOrgID = "018f0000-0000-7000-8000-0000000000a2"
	if _, err := get(unknownOrgID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("get link for unknown organization: got %v, want NotFound", err)
	}
}
