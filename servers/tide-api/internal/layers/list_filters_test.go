// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

func TestListFilterEnums(t *testing.T) {
	w := newWorld(t)
	client := w.env.User(w.org.Member)
	// A list query would wait on one of these tables until its context expires.
	holdTx(t, w.env, "LOCK TABLE runner_profiles, requests, runners IN ACCESS EXCLUSIVE MODE")

	kinds := []int32{4, 99, -1, 65536, 65537}
	states := []int32{9, 99, -1, 65536, 65537, 65538}
	filters := []struct {
		method string
		field  string
		values []int32
		call   func(context.Context, int32) error
	}{
		{"ListRunnerProfiles", "workload_kind", kinds, func(ctx context.Context, value int32) error {
			_, err := client.Profile.ListRunnerProfiles(ctx, connect.NewRequest(&tidev1.ListRunnerProfilesRequest{
				OrganizationId: w.org.ID, WorkloadKind: tidev1.WorkloadKind(value),
			}))
			return err
		}},
		{"ListRequests", "kind", kinds, func(ctx context.Context, value int32) error {
			_, err := client.Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{
				OrganizationId: w.org.ID, Kind: tidev1.WorkloadKind(value),
			}))
			return err
		}},
		{"ListRequests", "states", states, func(ctx context.Context, value int32) error {
			_, err := client.Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{
				OrganizationId: w.org.ID, States: []tidev1.RequestState{tidev1.RequestState_REQUEST_STATE_QUEUED, tidev1.RequestState(value)},
			}))
			return err
		}},
		{"ListRunners", "states", states, func(ctx context.Context, value int32) error {
			_, err := client.Work.ListRunners(ctx, connect.NewRequest(&tidev1.ListRunnersRequest{
				OrganizationId: w.org.ID, States: []tidev1.RunnerState{tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK, tidev1.RunnerState(value)},
			}))
			return err
		}},
		{"ListDevSessions", "states", states, func(ctx context.Context, value int32) error {
			_, err := client.DevSession.ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{
				OrganizationId: w.org.ID, States: []tidev1.RequestState{tidev1.RequestState_REQUEST_STATE_ENDED, tidev1.RequestState(value)}, IncludeEnded: true,
			}))
			return err
		}},
	}
	for _, filter := range filters {
		for _, value := range filter.values {
			t.Run(fmt.Sprintf("%s/%s/%d", filter.method, filter.field, value), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				err := filter.call(ctx, value)
				wantCode(t, filter.field, err, connect.CodeInvalidArgument)
				if !strings.HasPrefix(err.(*connect.Error).Message(), "invalid "+filter.field+":") {
					t.Fatalf("error %q does not name %s", err, filter.field)
				}
			})
		}
	}
}
