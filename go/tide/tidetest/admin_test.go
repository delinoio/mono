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

const (
	firstListProfile  = "018f0000-0000-7000-8000-00000000000a"
	secondListProfile = "018f0000-0000-7000-8000-00000000000b"
)

func listTestRequest[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+DefaultAccessToken)
	return req
}

func TestListRequestsFilters(t *testing.T) {
	const (
		job     = tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
		agent   = tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION
		queued  = tidev1.RequestState_REQUEST_STATE_QUEUED
		start   = tidev1.RequestState_REQUEST_STATE_STARTING
		running = tidev1.RequestState_REQUEST_STATE_RUNNING
		ended   = tidev1.RequestState_REQUEST_STATE_ENDED
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	for _, r := range []*tidev1.Request{
		{RequestId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, Kind: job, State: ended},
		{RequestId: "second-job", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: job, State: running},
		{RequestId: "first-agent", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: agent, State: start},
		{RequestId: "second-agent", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: agent, State: queued},
		{RequestId: "first-job", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: job, State: ended},
		{RequestId: "unspecified", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: agent},
	} {
		s.AddRequest(r)
	}
	client := tidev1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *tidev1.ListRequestsRequest
		want []string
	}{
		{"default open states", &tidev1.ListRequestsRequest{}, []string{"second-job", "first-agent", "second-agent"}},
		{"profile", &tidev1.ListRequestsRequest{RunnerProfileId: strings.ToUpper(firstListProfile), IncludeEnded: true}, []string{"first-agent", "first-job"}},
		{"kind", &tidev1.ListRequestsRequest{Kind: job, IncludeEnded: true}, []string{"second-job", "first-job"}},
		{"ended", &tidev1.ListRequestsRequest{States: []tidev1.RequestState{ended}}, []string{"first-job"}},
		{"ended with include_ended", &tidev1.ListRequestsRequest{States: []tidev1.RequestState{ended}, IncludeEnded: true}, []string{"first-job"}},
		{"intersection before pagination", &tidev1.ListRequestsRequest{RunnerProfileId: firstListProfile, Kind: job, IncludeEnded: true, PageSize: 1}, []string{"first-job"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.OrganizationId = org
			res, err := client.ListRequests(t.Context(), listTestRequest(tt.req))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range res.Msg.GetRequests() {
				ids = append(ids, row.GetRequestId())
			}
			if !slices.Equal(ids, tt.want) || res.Msg.GetNextPageToken() != "" {
				t.Errorf("got IDs %v and next token %q, want IDs %v and no next token", ids, res.Msg.GetNextPageToken(), tt.want)
			}
		})
	}
	_, err := client.ListRequests(t.Context(), listTestRequest(&tidev1.ListRequestsRequest{OrganizationId: org, RunnerProfileId: "invalid"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid profile: got %v, want InvalidArgument", err)
	}
}

func TestListRunnersFilters(t *testing.T) {
	const (
		busy     = tidev1.RunnerState_RUNNER_STATE_BUSY
		deleting = tidev1.RunnerState_RUNNER_STATE_DELETING
		failed   = tidev1.RunnerState_RUNNER_STATE_FAILED
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	for _, r := range []*tidev1.Runner{
		{RunnerId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, State: failed},
		{RunnerId: "second-failed", OrganizationId: org, RunnerProfileId: secondListProfile, State: failed},
		{RunnerId: "first-busy", OrganizationId: org, RunnerProfileId: firstListProfile, State: busy},
		{RunnerId: "second-deleting", OrganizationId: org, RunnerProfileId: secondListProfile, State: deleting},
		{RunnerId: "first-failed", OrganizationId: org, RunnerProfileId: firstListProfile, State: failed},
		{RunnerId: "unspecified", OrganizationId: org, RunnerProfileId: secondListProfile},
	} {
		s.AddRunner(r)
	}
	client := tidev1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *tidev1.ListRunnersRequest
		want []string
	}{
		{"default open states", &tidev1.ListRunnersRequest{}, []string{"first-busy", "second-deleting"}},
		{"profile", &tidev1.ListRunnersRequest{RunnerProfileId: strings.ToUpper(firstListProfile), IncludeEnded: true}, []string{"first-busy", "first-failed"}},
		{"failed", &tidev1.ListRunnersRequest{States: []tidev1.RunnerState{failed}}, []string{"second-failed", "first-failed"}},
		{"failed with include_ended", &tidev1.ListRunnersRequest{States: []tidev1.RunnerState{failed}, IncludeEnded: true}, []string{"second-failed", "first-failed"}},
		{"intersection before pagination", &tidev1.ListRunnersRequest{RunnerProfileId: firstListProfile, States: []tidev1.RunnerState{failed}, PageSize: 1}, []string{"first-failed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.OrganizationId = org
			res, err := client.ListRunners(t.Context(), listTestRequest(tt.req))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range res.Msg.GetRunners() {
				ids = append(ids, row.GetRunnerId())
			}
			if !slices.Equal(ids, tt.want) || res.Msg.GetNextPageToken() != "" {
				t.Errorf("got IDs %v and next token %q, want IDs %v and no next token", ids, res.Msg.GetNextPageToken(), tt.want)
			}
		})
	}
	_, err := client.ListRunners(t.Context(), listTestRequest(&tidev1.ListRunnersRequest{OrganizationId: org, RunnerProfileId: "018f0000-0000-4000-8000-00000000000a"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("UUID with wrong version: got %v, want InvalidArgument", err)
	}
}
