// SPDX-License-Identifier: Apache-2.0

package tidetest

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

func TestListDevSessionsFilters(t *testing.T) {
	const (
		running    = tidev1.RequestState_REQUEST_STATE_RUNNING
		ended      = tidev1.RequestState_REQUEST_STATE_ENDED
		otherOwner = "user-2"
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	s.mu.Lock()
	s.sessions = []*tidev1.DevSession{
		{DevSessionId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "second-ended", OrganizationId: org, RunnerProfileId: secondListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "other-owner", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: otherOwner, State: ended},
		{DevSessionId: "first-running", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: running},
		{DevSessionId: "first-ended", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "unspecified", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID},
	}
	s.mu.Unlock()
	client := tidev1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *tidev1.ListDevSessionsRequest
		want []string
	}{
		{"default open states", &tidev1.ListDevSessionsRequest{}, []string{"first-running"}},
		{"profile", &tidev1.ListDevSessionsRequest{RunnerProfileId: strings.ToUpper(secondListProfile), IncludeEnded: true}, []string{"second-ended"}},
		{"ended", &tidev1.ListDevSessionsRequest{States: []tidev1.RequestState{ended}}, []string{"second-ended", "other-owner", "first-ended"}},
		{"ended with include_ended", &tidev1.ListDevSessionsRequest{States: []tidev1.RequestState{ended}, IncludeEnded: true}, []string{"second-ended", "other-owner", "first-ended"}},
		{
			"caller ownership intersection before pagination",
			&tidev1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				States:          []tidev1.RequestState{ended},
				Owner:           &tidev1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true},
				PageSize:        1,
			},
			[]string{"first-ended"},
		},
		{
			"explicit owner intersection",
			&tidev1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				Owner:           &tidev1.ListDevSessionsRequest_OwnerUserId{OwnerUserId: otherOwner},
				IncludeEnded:    true,
			},
			[]string{"other-owner"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.OrganizationId = org
			res, err := client.ListDevSessions(t.Context(), listTestRequest(tt.req))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range res.Msg.GetDevSessions() {
				ids = append(ids, row.GetDevSessionId())
			}
			if !slices.Equal(ids, tt.want) || res.Msg.GetNextPageToken() != "" {
				t.Errorf("got IDs %v and next token %q, want IDs %v and no next token", ids, res.Msg.GetNextPageToken(), tt.want)
			}
		})
	}
	_, err := client.ListDevSessions(t.Context(), listTestRequest(&tidev1.ListDevSessionsRequest{OrganizationId: org, RunnerProfileId: " "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("whitespace-only profile: got %v, want InvalidArgument", err)
	}
}
