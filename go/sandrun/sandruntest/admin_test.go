// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
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
		job     = sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
		agent   = sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION
		queued  = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		start   = sandrunv1.RequestState_REQUEST_STATE_STARTING
		running = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		ended   = sandrunv1.RequestState_REQUEST_STATE_ENDED
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	for _, r := range []*sandrunv1.Request{
		{RequestId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, Kind: job, State: ended},
		{RequestId: "second-job", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: job, State: running},
		{RequestId: "first-agent", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: agent, State: start},
		{RequestId: "second-agent", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: agent, State: queued},
		{RequestId: "first-job", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: job, State: ended},
		{RequestId: "unspecified", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: agent},
	} {
		s.AddRequest(r)
	}
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *sandrunv1.ListRequestsRequest
		want []string
	}{
		{"default open states", &sandrunv1.ListRequestsRequest{}, []string{"second-job", "first-agent", "second-agent"}},
		{"profile", &sandrunv1.ListRequestsRequest{RunnerProfileId: strings.ToUpper(firstListProfile), IncludeEnded: true}, []string{"first-agent", "first-job"}},
		{"kind", &sandrunv1.ListRequestsRequest{Kind: job, IncludeEnded: true}, []string{"second-job", "first-job"}},
		{"ended", &sandrunv1.ListRequestsRequest{States: []sandrunv1.RequestState{ended}}, []string{"first-job"}},
		{"ended with include_ended", &sandrunv1.ListRequestsRequest{States: []sandrunv1.RequestState{ended}, IncludeEnded: true}, []string{"first-job"}},
		{"intersection before pagination", &sandrunv1.ListRequestsRequest{RunnerProfileId: firstListProfile, Kind: job, IncludeEnded: true, PageSize: 1}, []string{"first-job"}},
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
	_, err := client.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{OrganizationId: org, RunnerProfileId: "invalid"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid profile: got %v, want InvalidArgument", err)
	}
}

func TestListRunnersFilters(t *testing.T) {
	const (
		busy     = sandrunv1.RunnerState_RUNNER_STATE_BUSY
		deleting = sandrunv1.RunnerState_RUNNER_STATE_DELETING
		failed   = sandrunv1.RunnerState_RUNNER_STATE_FAILED
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	for _, r := range []*sandrunv1.Runner{
		{RunnerId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, State: failed},
		{RunnerId: "second-failed", OrganizationId: org, RunnerProfileId: secondListProfile, State: failed},
		{RunnerId: "first-busy", OrganizationId: org, RunnerProfileId: firstListProfile, State: busy},
		{RunnerId: "second-deleting", OrganizationId: org, RunnerProfileId: secondListProfile, State: deleting},
		{RunnerId: "first-failed", OrganizationId: org, RunnerProfileId: firstListProfile, State: failed},
		{RunnerId: "unspecified", OrganizationId: org, RunnerProfileId: secondListProfile},
	} {
		s.AddRunner(r)
	}
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *sandrunv1.ListRunnersRequest
		want []string
	}{
		{"default open states", &sandrunv1.ListRunnersRequest{}, []string{"first-busy", "second-deleting"}},
		{"profile", &sandrunv1.ListRunnersRequest{RunnerProfileId: strings.ToUpper(firstListProfile), IncludeEnded: true}, []string{"first-busy", "first-failed"}},
		{"failed", &sandrunv1.ListRunnersRequest{States: []sandrunv1.RunnerState{failed}}, []string{"second-failed", "first-failed"}},
		{"failed with include_ended", &sandrunv1.ListRunnersRequest{States: []sandrunv1.RunnerState{failed}, IncludeEnded: true}, []string{"second-failed", "first-failed"}},
		{"intersection before pagination", &sandrunv1.ListRunnersRequest{RunnerProfileId: firstListProfile, States: []sandrunv1.RunnerState{failed}, PageSize: 1}, []string{"first-failed"}},
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
	_, err := client.ListRunners(t.Context(), listTestRequest(&sandrunv1.ListRunnersRequest{OrganizationId: org, RunnerProfileId: "018f0000-0000-4000-8000-00000000000a"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("UUID with wrong version: got %v, want InvalidArgument", err)
	}
}

func TestListApiKeysExpiryBoundary(t *testing.T) {
	const (
		active  = sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE
		expired = sandrunv1.ApiKeyState_API_KEY_STATE_EXPIRED
	)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		expires := start.Add(time.Nanosecond)
		key := &sandrunv1.ApiKey{
			ApiKeyId: "key-1", OrganizationId: "org-1", Name: "test key",
			DisplayPrefix: "sandrun_abcd", State: active,
			CreatedAt: timestamppb.New(start), ExpiresAt: timestamppb.New(expires),
		}
		stored := proto.CloneOf(key)
		service := &apiKeyService{s: &Server{apiKeys: []*sandrunv1.ApiKey{key}}}
		req := connect.NewRequest(&sandrunv1.ListApiKeysRequest{OrganizationId: key.GetOrganizationId()})
		for _, tt := range []struct {
			offset time.Duration
			state  sandrunv1.ApiKeyState
		}{
			{-time.Nanosecond, active},
			{0, expired},
			{time.Nanosecond, expired},
		} {
			time.Sleep(time.Until(expires.Add(tt.offset)))
			res, err := service.ListApiKeys(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			keys := res.Msg.GetApiKeys()
			if len(keys) != 1 {
				t.Fatalf("at expiry %+v: got %d keys, want 1", tt.offset, len(keys))
			}
			want := proto.CloneOf(stored)
			want.State = tt.state
			if keys[0] == key || !proto.Equal(keys[0], want) {
				t.Errorf("at expiry %+v: got %v, want a clone equal to %v", tt.offset, keys[0], want)
			}
			if !proto.Equal(key, stored) {
				t.Errorf("at expiry %+v: stored key changed to %v", tt.offset, key)
			}
		}
	})
}
