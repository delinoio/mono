// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func openQueueWatch(t *testing.T, ctx context.Context, client sandrunv1connect.WorkServiceClient, org string) *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse] {
	t.Helper()
	stream, err := client.WatchQueue(ctx, listTestRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if !stream.Receive() || stream.Msg().GetSnapshotComplete() == nil {
		t.Fatalf("watch for %s: got %v (error: %v), want snapshot marker", org, stream.Msg(), stream.Err())
	}
	return stream
}

func queueRequest(org, id string) *sandrunv1.WatchQueueResponse {
	return &sandrunv1.WatchQueueResponse{
		Event: &sandrunv1.WatchQueueResponse_Request{Request: &sandrunv1.Request{
			OrganizationId: org, RequestId: id,
		}},
	}
}

func queueRunner(org, id string) *sandrunv1.WatchQueueResponse {
	return &sandrunv1.WatchQueueResponse{
		Event: &sandrunv1.WatchQueueResponse_Runner{Runner: &sandrunv1.Runner{
			OrganizationId: org, RunnerId: id,
		}},
	}
}

func requireQueueUpdate(t *testing.T, stream *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse], want *sandrunv1.WatchQueueResponse) {
	t.Helper()
	if !stream.Receive() || !proto.Equal(stream.Msg(), want) {
		t.Fatalf("got %v (error: %v), want %v", stream.Msg(), stream.Err(), want)
	}
}

func requireQueueEOF(t *testing.T, stream *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse]) {
	t.Helper()
	if stream.Receive() || stream.Err() != nil {
		t.Fatalf("got %v (error: %v), want successful end of stream", stream.Msg(), stream.Err())
	}
}

func TestWatchQueueRoutesUpdatesByOrganization(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	first := s.AddOrganization("first").GetOrganizationId()
	second := s.AddOrganization("second").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)

	firstWatch := openQueueWatch(t, ctx, client, first)
	secondWatch := openQueueWatch(t, ctx, client, second)

	firstRequest := queueRequest(first, "first-request")
	firstRunner := queueRunner(first, "first-runner")
	secondRequest := queueRequest(second, "second-request")
	secondRunner := queueRunner(second, "second-runner")
	for _, event := range []*sandrunv1.WatchQueueResponse{firstRequest, secondRunner, firstRunner, secondRequest} {
		s.SendQueueEvent(event)
	}

	requireQueueUpdate(t, firstWatch, firstRequest)
	requireQueueUpdate(t, firstWatch, firstRunner)
	requireQueueUpdate(t, secondWatch, secondRunner)
	requireQueueUpdate(t, secondWatch, secondRequest)
}

func TestWatchQueueCountsOnlyUpdatesSentToTheWatch(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	s.WatchEventLimit = 2 // snapshot marker and one update
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	first := s.AddOrganization("first").GetOrganizationId()
	second := s.AddOrganization("second").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)

	firstWatch := openQueueWatch(t, ctx, client, first)
	firstRequest := queueRequest(first, "first-request")
	secondRunner := queueRunner(second, "second-runner")
	s.SendQueueEvent(secondRunner)
	s.SendQueueEvent(firstRequest)

	requireQueueUpdate(t, firstWatch, firstRequest)
	requireQueueEOF(t, firstWatch)

	secondWatch := openQueueWatch(t, ctx, client, second)
	requireQueueUpdate(t, secondWatch, secondRunner)
	requireQueueEOF(t, secondWatch)
}

func TestWatchQueueSendsEachUpdateToOneWatch(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	s.WatchEventLimit = 2 // each watch takes one update
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	org := s.AddOrganization("shared").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	firstWatch := openQueueWatch(t, ctx, client, org)
	secondWatch := openQueueWatch(t, ctx, client, org)

	request := queueRequest(org, "request")
	runner := queueRunner(org, "runner")
	s.SendQueueEvent(request)
	s.SendQueueEvent(runner)

	if !firstWatch.Receive() {
		t.Fatalf("first watch ended before the update: %v", firstWatch.Err())
	}
	switch received := firstWatch.Msg(); {
	case proto.Equal(received, request):
		requireQueueUpdate(t, secondWatch, runner)
	case proto.Equal(received, runner):
		requireQueueUpdate(t, secondWatch, request)
	default:
		t.Fatalf("first watch got unexpected update %v", received)
	}
	requireQueueEOF(t, firstWatch)
	requireQueueEOF(t, secondWatch)
}
