// SPDX-License-Identifier: Apache-2.0

package sandruntest_test

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestUpdateOrganizationSettings(t *testing.T) {
	limitMask := &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}}
	positive := &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(2)}
	empty := &sandrunv1.OrganizationSettings{}
	for _, tt := range []struct {
		name     string
		mask     *fieldmaskpb.FieldMask
		settings *sandrunv1.OrganizationSettings
		want     *sandrunv1.OrganizationSettings
		invalid  bool
	}{
		{"omitted settings", limitMask, nil, empty, false},
		{"empty settings", limitMask, empty, empty, false},
		{"zero limit", limitMask, &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(0)}, positive, true},
		{"negative limit", limitMask, &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(-1)}, positive, true},
		{"omitted mask", nil, nil, positive, true},
		{"empty mask", &fieldmaskpb.FieldMask{}, nil, positive, true},
		{"unknown path", &fieldmaskpb.FieldMask{Paths: []string{"unknown"}}, nil, positive, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := sandruntest.New()
			t.Cleanup(server.Close)
			orgID := server.AddOrganization("test").GetOrganizationId()
			client, err := sandrun.New(sandrun.Options{BaseURL: server.URL, APIKey: server.AccessToken})
			if err != nil {
				t.Fatal(err)
			}
			update := func(mask *fieldmaskpb.FieldMask, settings *sandrunv1.OrganizationSettings) (*connect.Response[sandrunv1.UpdateOrganizationSettingsResponse], error) {
				return client.Organizations().UpdateOrganizationSettings(t.Context(), connect.NewRequest(&sandrunv1.UpdateOrganizationSettingsRequest{
					OrganizationId: orgID,
					UpdateMask:     mask,
					Settings:       settings,
				}))
			}
			seed, err := update(limitMask, positive)
			if err != nil {
				t.Fatalf("set positive limit: %v", err)
			}
			if got := seed.Msg.GetOrganization().GetSettings(); !proto.Equal(got, positive) {
				t.Fatalf("set limit response settings = %v, want %v", got, positive)
			}
			if got := server.Organizations()[0].GetSettings(); !proto.Equal(got, positive) {
				t.Fatalf("settings after setting limit = %v, want %v", got, positive)
			}

			res, err := update(tt.mask, tt.settings)
			if tt.invalid {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("update settings error = %v, want InvalidArgument", err)
				}
			} else {
				if err != nil {
					t.Fatalf("update settings: %v", err)
				}
				org := res.Msg.GetOrganization()
				if org.GetOrganizationId() != orgID || !proto.Equal(org.GetSettings(), tt.want) {
					t.Errorf("update response = %v, want organization %s with settings %v", org, orgID, tt.want)
				}
			}
			if got := server.Organizations()[0].GetSettings(); !proto.Equal(got, tt.want) {
				t.Errorf("stored settings = %v, want %v", got, tt.want)
			}
		})
	}
}
