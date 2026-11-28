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

func assertInvalidListFilter(t *testing.T, err error, message string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+message {
		t.Errorf("got %v, want invalid_argument: %s", err, message)
	}
}

func TestListRunnerProfilesFilters(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	profile := s.AddDevSessionProfile(org, "dev", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
	client := sandrunv1connect.NewRunnerProfileServiceClient(http.DefaultClient, s.URL)

	for _, tt := range []struct {
		kind sandrunv1.WorkloadKind
		want []string
	}{
		{sandrunv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED, []string{profile.GetRunnerProfileId()}},
		{sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, []string{profile.GetRunnerProfileId()}},
		{sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, nil},
	} {
		t.Run(tt.kind.String(), func(t *testing.T) {
			res, err := client.ListRunnerProfiles(t.Context(), listTestRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: org, WorkloadKind: tt.kind}))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range res.Msg.GetProfiles() {
				ids = append(ids, row.GetRunnerProfileId())
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("got profile IDs %v, want %v", ids, tt.want)
			}
		})
	}
	_, err := client.ListRunnerProfiles(t.Context(), listTestRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: org, WorkloadKind: 99}))
	assertInvalidListFilter(t, err, "invalid workload_kind: must be a defined WorkloadKind value")
}

func TestListRequestsFilters(t *testing.T) {
	const (
		job     = sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
		agent   = sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION
		normal  = sandrunv1.Priority_PRIORITY_NORMAL
		queued  = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		start   = sandrunv1.RequestState_REQUEST_STATE_STARTING
		running = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		ended   = sandrunv1.RequestState_REQUEST_STATE_ENDED
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	earlier := timestamppb.New(time.Unix(100, 0))
	later := timestamppb.New(time.Unix(200, 0))
	for _, r := range []*sandrunv1.Request{
		{RequestId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, Kind: job, State: ended},
		{RequestId: "second-job", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: job, State: running, Priority: normal, QueuedAt: earlier},
		{RequestId: "first-agent", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: agent, State: start, Priority: normal, QueuedAt: later},
		{RequestId: "second-agent", OrganizationId: org, RunnerProfileId: secondListProfile, Kind: agent, State: queued, Priority: normal, QueuedAt: later},
		{RequestId: "first-job", OrganizationId: org, RunnerProfileId: firstListProfile, Kind: job, State: ended, EndedAt: later},
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
		{"defined zero state", &sandrunv1.ListRequestsRequest{States: []sandrunv1.RequestState{sandrunv1.RequestState_REQUEST_STATE_UNSPECIFIED}}, []string{"unspecified"}},
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
	_, err = client.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{OrganizationId: org, Kind: 99}))
	assertInvalidListFilter(t, err, "invalid kind: must be a defined WorkloadKind value")
	for _, states := range [][]sandrunv1.RequestState{{99}, {running, 99}} {
		for _, includeEnded := range []bool{false, true} {
			_, err := client.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{OrganizationId: org, States: states, IncludeEnded: includeEnded}))
			assertInvalidListFilter(t, err, "invalid states: must be a defined RequestState value")
		}
	}
}

func TestListRequestsOrderAcrossPages(t *testing.T) {
	t.Parallel()
	const (
		queued   = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		starting = sandrunv1.RequestState_REQUEST_STATE_STARTING
		running  = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		ended    = sandrunv1.RequestState_REQUEST_STATE_ENDED
		canceled = sandrunv1.RequestState_REQUEST_STATE_CANCELED
		expired  = sandrunv1.RequestState_REQUEST_STATE_EXPIRED
		failed   = sandrunv1.RequestState_REQUEST_STATE_FAILED
		high     = sandrunv1.Priority_PRIORITY_HIGH
		normal   = sandrunv1.Priority_PRIORITY_NORMAL
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("queue").GetOrganizationId()
	earlier := timestamppb.New(time.Unix(100, 0))
	later := timestamppb.New(time.Unix(100, 1))
	// Seed in reverse of the production query order.
	for _, r := range []*sandrunv1.Request{
		{RequestId: "ended-old", State: ended, EndedAt: earlier},
		{RequestId: "ended-b", State: canceled, EndedAt: later},
		{RequestId: "ended-a", State: expired, EndedAt: later},
		{RequestId: "no-end", State: failed},
		{RequestId: "unknown-b", State: 99, Priority: high, QueuedAt: earlier},
		{RequestId: "unknown-a", State: sandrunv1.RequestState_REQUEST_STATE_UNSPECIFIED},
		{RequestId: "normal", State: queued, Priority: normal, QueuedAt: earlier},
		{RequestId: "no-queue", State: queued, Priority: high},
		{RequestId: "high-later", State: running, Priority: high, QueuedAt: later},
		{RequestId: "high-b", State: starting, Priority: high, QueuedAt: earlier},
		{RequestId: "high-a", State: queued, Priority: high, QueuedAt: earlier},
	} {
		r.OrganizationId = org
		r.RunnerProfileId = firstListProfile
		r.Kind = sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
		s.AddRequest(r)
	}

	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	want := []string{
		"high-a", "high-b", "high-later", "no-queue", "normal",
		"unknown-a", "unknown-b", "no-end", "ended-a", "ended-b", "ended-old",
	}
	var token string
	for i, id := range want {
		res, err := client.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{
			OrganizationId: org,
			IncludeEnded:   true,
			PageSize:       1,
			PageToken:      token,
		}))
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		rows := res.Msg.GetRequests()
		if len(rows) != 1 || rows[0].GetRequestId() != id {
			t.Fatalf("page %d: got %v, want request %q", i+1, rows, id)
		}
		token = res.Msg.GetNextPageToken()
		if (token == "") != (i == len(want)-1) {
			t.Fatalf("page %d returned unexpected next token %q", i+1, token)
		}
	}
}

func requireDevSessionWorkRequest(t *testing.T, got *sandrunv1.Request, ds *sandrunv1.DevSession, priority sandrunv1.Priority) {
	t.Helper()
	want := &sandrunv1.Request{
		RequestId:       ds.GetDevSessionId(),
		OrganizationId:  ds.GetOrganizationId(),
		RunnerProfileId: ds.GetRunnerProfileId(),
		OwnerUserId:     ds.GetOwnerUserId(),
		Kind:            sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION,
		Priority:        priority,
		State:           ds.GetState(),
		EndReason:       ds.GetEndReason(),
		QueuedAt:        ds.GetQueuedAt(),
		StartedAt:       ds.GetStartedAt(),
		EndedAt:         ds.GetEndedAt(),
		RunnerId:        ds.GetRunnerId(),
	}
	if !proto.Equal(got, want) {
		t.Fatalf("work request = %v, want %v", got, want)
	}
}

func TestWorkRequestCancelsQueuedDevSession(t *testing.T) {
	t.Parallel()
	const (
		queued = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		job    = sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
		high   = sandrunv1.Priority_PRIORITY_HIGH
	)
	s := New()
	t.Cleanup(s.Close)
	s.NewSessionState = queued
	org := s.AddOrganization("session owner").GetOrganizationId()
	other := s.AddOrganization("other").GetOrganizationId()
	profiles := sandrunv1connect.NewRunnerProfileServiceClient(http.DefaultClient, s.URL)
	dev := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	work := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	profile, err := profiles.CreateRunnerProfile(t.Context(), listTestRequest(&sandrunv1.CreateRunnerProfileRequest{
		OrganizationId: org,
		Profile: &sandrunv1.RunnerProfile{
			Name: "development", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION,
			GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Priority: high, WorkspacePath: t.TempDir(),
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	profileID := profile.Msg.GetProfile().GetRunnerProfileId()
	created, err := dev.CreateDevSession(t.Context(), listTestRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profileID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ds := created.Msg.GetDevSession()
	id := ds.GetDevSessionId()
	if ds.GetState() != queued || ds.GetQueuedAt() == nil {
		t.Fatalf("unexpected queued session: %v", ds)
	}
	getWork := func(orgID string) (*sandrunv1.Request, error) {
		res, err := work.GetRequest(t.Context(), listTestRequest(&sandrunv1.GetRequestRequest{OrganizationId: orgID, RequestId: id}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetRequest(), nil
	}
	cancel := func(orgID string) (*sandrunv1.Request, error) {
		res, err := work.CancelRequest(t.Context(), listTestRequest(&sandrunv1.CancelRequestRequest{OrganizationId: orgID, RequestId: id}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetRequest(), nil
	}

	r, err := getWork(org)
	if err != nil {
		t.Fatal(err)
	}
	requireDevSessionWorkRequest(t, r, ds, high)

	// A later profile change must not change the saved priority of the session.
	s.mu.Lock()
	s.profiles[0].Priority = sandrunv1.Priority_PRIORITY_LOW
	s.mu.Unlock()
	seed := &sandrunv1.Request{RequestId: id, OrganizationId: org, Kind: job, State: queued}
	s.AddRequest(seed)
	r, err = getWork(org)
	if err != nil {
		t.Fatal(err)
	}
	requireDevSessionWorkRequest(t, r, ds, high)

	list, err := work.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{
		OrganizationId: org, RunnerProfileId: profileID, Kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION,
		States: []sandrunv1.RequestState{queued}, PageSize: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows := list.Msg.GetRequests(); len(rows) != 1 || !proto.Equal(rows[0], r) || list.Msg.GetNextPageToken() != "" {
		t.Fatalf("filtered work list = %v, want one projected request", list.Msg)
	}

	if _, err := getWork(other); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("cross-organization lookup = %v, want not_found", err)
	}
	if _, err := cancel(other); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("cross-organization cancellation = %v, want not_found", err)
	}
	// An identical ID in another organization still refers to its own seed.
	seed.OrganizationId = other
	s.AddRequest(seed)
	standalone, err := getWork(other)
	if err != nil || !proto.Equal(standalone, seed) {
		t.Fatalf("standalone work lookup = %v, %v, want %v", standalone, err, seed)
	}
	standalone, err = cancel(other)
	if err != nil || standalone.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
		standalone.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER || standalone.GetEndedAt() == nil {
		t.Fatalf("standalone work cancellation = %v, %v", standalone, err)
	}

	canceled, err := cancel(org)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
		canceled.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER || canceled.GetEndedAt() == nil {
		t.Fatalf("unexpected canceled request: %v", canceled)
	}
	session, err := dev.GetDevSession(t.Context(), listTestRequest(&sandrunv1.GetDevSessionRequest{OrganizationId: org, DevSessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	requireDevSessionWorkRequest(t, canceled, session.Msg.GetDevSession(), high)
	if _, err := cancel(org); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("repeat cancellation = %v, want failed_precondition", err)
	}
	if current, err := getWork(org); err != nil || !proto.Equal(current, canceled) {
		t.Errorf("work request after repeat cancellation = %v, %v, want %v", current, err, canceled)
	}
	for _, includeEnded := range []bool{false, true} {
		list, err := work.ListRequests(t.Context(), listTestRequest(&sandrunv1.ListRequestsRequest{OrganizationId: org, IncludeEnded: includeEnded}))
		if err != nil {
			t.Fatal(err)
		}
		rows := list.Msg.GetRequests()
		if (!includeEnded && len(rows) != 0) || (includeEnded && (len(rows) != 1 || !proto.Equal(rows[0], canceled))) {
			t.Errorf("work list with include_ended=%t = %v", includeEnded, list.Msg)
		}
	}
}

func TestWorkRequestFollowsDevSessionPromotionAndDeletion(t *testing.T) {
	t.Parallel()
	s := New()
	t.Cleanup(s.Close)
	s.NewSessionState = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	s.PromoteAfterGets = 1
	dev, ds := createExecTestSession(t, t.Context(), s, t.TempDir())
	work := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	org, id := ds.GetOrganizationId(), ds.GetDevSessionId()
	getWork := func() *sandrunv1.Request {
		t.Helper()
		res, err := work.GetRequest(t.Context(), listTestRequest(&sandrunv1.GetRequestRequest{OrganizationId: org, RequestId: id}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetRequest()
	}

	promoted, err := dev.GetDevSession(t.Context(), listTestRequest(&sandrunv1.GetDevSessionRequest{OrganizationId: org, DevSessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	running := promoted.Msg.GetDevSession()
	if running.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING || running.GetRunnerId() == "" || running.GetStartedAt() == nil {
		t.Fatalf("unexpected promoted session: %v", running)
	}
	const priority = sandrunv1.Priority_PRIORITY_NORMAL
	requireDevSessionWorkRequest(t, getWork(), running, priority)

	_, err = work.CancelRequest(t.Context(), listTestRequest(&sandrunv1.CancelRequestRequest{OrganizationId: org, RequestId: id}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("running cancellation = %v, want failed_precondition", err)
	}
	requireDevSessionWorkRequest(t, getWork(), running, priority)

	deleted, err := dev.DeleteDevSession(t.Context(), listTestRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: org, DevSessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	ended := deleted.Msg.GetDevSession()
	if ended.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		ended.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED || ended.GetEndedAt() == nil {
		t.Fatalf("unexpected deleted session: %v", ended)
	}
	requireDevSessionWorkRequest(t, getWork(), ended, priority)
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
		{"defined zero state", &sandrunv1.ListRunnersRequest{States: []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_UNSPECIFIED}}, []string{"unspecified"}},
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
	for _, states := range [][]sandrunv1.RunnerState{{99}, {busy, 99}} {
		for _, includeEnded := range []bool{false, true} {
			_, err := client.ListRunners(t.Context(), listTestRequest(&sandrunv1.ListRunnersRequest{OrganizationId: org, States: states, IncludeEnded: includeEnded}))
			assertInvalidListFilter(t, err, "invalid states: must be a defined RunnerState value")
		}
	}
}

func TestCreateApiKeyExpiry(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("test").GetOrganizationId()
	client := sandrunv1connect.NewApiKeyServiceClient(http.DefaultClient, s.URL)
	create := func(t *testing.T, expiresAt *timestamppb.Timestamp) (*connect.Response[sandrunv1.CreateApiKeyResponse], error) {
		t.Helper()
		return client.CreateApiKey(t.Context(), listTestRequest(&sandrunv1.CreateApiKeyRequest{
			OrganizationId: org, Name: "test key", ExpiresAt: expiresAt,
		}))
	}
	list := func(t *testing.T) *sandrunv1.ListApiKeysResponse {
		t.Helper()
		res, err := client.ListApiKeys(t.Context(), listTestRequest(&sandrunv1.ListApiKeysRequest{OrganizationId: org}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}

	future := timestamppb.New(time.Now().Add(time.Hour))
	for _, expiresAt := range []*timestamppb.Timestamp{nil, future} {
		res, err := create(t, expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetApiKey().GetExpiresAt(); !proto.Equal(got, expiresAt) {
			t.Errorf("created key expiry = %v, want %v", got, expiresAt)
		}
	}
	before := list(t)
	if got := len(before.GetApiKeys()); got != 2 {
		t.Fatalf("listed %d keys after successful creation, want 2", got)
	}
	for _, tt := range []struct {
		name      string
		expiresAt *timestamppb.Timestamp
	}{
		{"past", timestamppb.New(time.Now().Add(-time.Hour))},
		{"now", timestamppb.Now()},
		{"invalid nanoseconds", &timestamppb.Timestamp{Seconds: future.GetSeconds(), Nanos: 1_000_000_000}},
		{"out-of-range seconds", timestamppb.New(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := create(t, tt.expiresAt)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("create key: %v, want invalid_argument", err)
			}
			if after := list(t); !proto.Equal(after, before) {
				t.Errorf("keys after rejected creation = %v, want %v", after, before)
			}
		})
	}
}

func TestCreateApiKeyExpiryAtIssuance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const org = "org-1"
		s := &Server{orgs: []*sandrunv1.Organization{{OrganizationId: org}}}
		service := &apiKeyService{s: s}
		now := time.Now()
		req := connect.NewRequest(&sandrunv1.CreateApiKeyRequest{
			OrganizationId: org, Name: "test key", ExpiresAt: timestamppb.New(now),
		})
		_, err := service.CreateApiKey(t.Context(), req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || len(s.apiKeys) != 0 {
			t.Fatalf("create with expiry equal to issuance time: got %v and %d keys, want invalid_argument and no keys", err, len(s.apiKeys))
		}

		req.Msg.ExpiresAt = timestamppb.New(now.Add(time.Nanosecond))
		res, err := service.CreateApiKey(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetApiKey().GetCreatedAt(); !proto.Equal(got, timestamppb.New(now)) {
			t.Errorf("created_at = %v, want %v", got, now)
		}
	})
}

func TestExpiredApiKeyAuthentication(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("test").GetOrganizationId()
	admin := sandrunv1connect.NewApiKeyServiceClient(http.DefaultClient, s.URL)
	key, err := admin.CreateApiKey(t.Context(), listTestRequest(&sandrunv1.CreateApiKeyRequest{
		OrganizationId: org, Name: "test key", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	list := func() error {
		req := connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: org})
		req.Header().Set("Authorization", "Bearer "+key.Msg.GetSecret())
		_, err := client.ListDevSessions(t.Context(), req)
		return err
	}
	if err := list(); err != nil {
		t.Fatalf("list with unexpired key: %v", err)
	}

	s.mu.Lock()
	s.apiKeys[0].ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour))
	s.mu.Unlock()

	if err := list(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("list with expired key: %v, want unauthenticated", err)
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

func TestRevokeApiKeyPreservesTimestamp(t *testing.T) {
	const revoked = sandrunv1.ApiKeyState_API_KEY_STATE_REVOKED
	req := connect.NewRequest(&sandrunv1.RevokeApiKeyRequest{OrganizationId: "org-1", ApiKeyId: "key-1"})
	t.Run("first revoke", func(t *testing.T) {
		key := &sandrunv1.ApiKey{
			OrganizationId: req.Msg.GetOrganizationId(), ApiKeyId: req.Msg.GetApiKeyId(),
			State: sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE,
		}
		service := &apiKeyService{s: &Server{apiKeys: []*sandrunv1.ApiKey{key}}}

		res, err := service.RevokeApiKey(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetApiKey(); got.GetState() != revoked || got.GetRevokedAt() == nil {
			t.Fatalf("got key %v, want REVOKED with a timestamp", got)
		}
	})

	t.Run("retry and list", func(t *testing.T) {
		key := &sandrunv1.ApiKey{
			OrganizationId: req.Msg.GetOrganizationId(), ApiKeyId: req.Msg.GetApiKeyId(),
			State: revoked, RevokedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		}
		want := proto.CloneOf(key)
		service := &apiKeyService{s: &Server{apiKeys: []*sandrunv1.ApiKey{key}}}

		res, err := service.RevokeApiKey(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetApiKey(); !proto.Equal(got, want) {
			t.Errorf("retry returned %v, want %v", got, want)
		}

		list, err := service.ListApiKeys(t.Context(), connect.NewRequest(&sandrunv1.ListApiKeysRequest{
			OrganizationId: req.Msg.GetOrganizationId(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		if keys := list.Msg.GetApiKeys(); len(keys) != 1 || !proto.Equal(keys[0], want) {
			t.Errorf("list returned %v, want only %v", keys, want)
		}

		for _, missing := range []*sandrunv1.RevokeApiKeyRequest{
			{OrganizationId: req.Msg.GetOrganizationId(), ApiKeyId: "unknown"},
			{OrganizationId: "org-2", ApiKeyId: req.Msg.GetApiKeyId()},
		} {
			_, err := service.RevokeApiKey(t.Context(), connect.NewRequest(missing))
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("revoke %v returned %v, want NotFound", missing, err)
			}
		}
	})
}
