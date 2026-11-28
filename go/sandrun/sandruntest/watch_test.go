// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
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

func queueKeepalive(sequence int) *sandrunv1.WatchQueueResponse {
	keepalive := &sandrunv1.WatchQueueKeepalive{}
	// Keepalives have no declared fields. Use an unknown field to verify order.
	tag := protowire.AppendTag(nil, 99, protowire.VarintType)
	keepalive.ProtoReflect().SetUnknown(protowire.AppendVarint(tag, uint64(sequence)))
	return &sandrunv1.WatchQueueResponse{
		Event: &sandrunv1.WatchQueueResponse_Keepalive{Keepalive: keepalive},
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

func TestWatchQueueQueuesBurstBeforeWatch(t *testing.T) {
	t.Parallel()

	const updates = 65
	s := New()
	t.Cleanup(s.Close)
	s.WatchEventLimit = updates + 1 // snapshot marker and all queued updates
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	org := s.AddOrganization("burst").GetOrganizationId()

	enqueued := make(chan struct{})
	go func() {
		defer close(enqueued)
		update := queueRequest(org, "")
		for i := range updates {
			update.GetRequest().RequestId = strconv.Itoa(i)
			s.SendQueueEvent(update)
		}
		update.GetRequest().RequestId = "after-enqueue"
	}()
	select {
	case <-enqueued:
	case <-ctx.Done():
		t.Fatal("queuing 65 scoped updates blocked before the watch started")
	}

	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	watch := openQueueWatch(t, ctx, client, org)
	for i := range updates {
		requireQueueUpdate(t, watch, queueRequest(org, strconv.Itoa(i)))
	}
	requireQueueEOF(t, watch)
}

func TestWatchQueueQueuesUnscopedBurstBeforeWatch(t *testing.T) {
	t.Parallel()

	const updates = 65
	s := New()
	t.Cleanup(s.Close)
	s.WatchEventLimit = updates + 1
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	org := s.AddOrganization("burst").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)

	enqueued := make(chan struct{})
	go func() {
		defer close(enqueued)
		for i := range updates {
			s.SendQueueEvent(queueKeepalive(i))
		}
	}()
	select {
	case <-enqueued:
	case <-ctx.Done():
		// Drain the queue so a blocked sender can finish if the fixed
		// buffer returns.
		drainCtx, cancelDrain := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelDrain()
		drain := openQueueWatch(t, drainCtx, client, org)
		for drain.Receive() {
		}
		t.Fatal("queuing 65 unscoped events blocked before the watch started")
	}

	watch := openQueueWatch(t, ctx, client, org)
	for i := range updates {
		requireQueueUpdate(t, watch, queueKeepalive(i))
	}
	requireQueueEOF(t, watch)
}

func TestWatchQueueSendsUnscopedEventToOneWatch(t *testing.T) {
	t.Parallel()

	s := New()
	t.Cleanup(s.Close)
	s.WatchEventLimit = 2
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	type receipt struct {
		index int
		event *sandrunv1.WatchQueueResponse
		extra bool
		err   error
	}
	results := make(chan receipt, 2)
	var cancels [2]context.CancelFunc
	for i, name := range []string{"first", "second"} {
		watchCtx, cancelWatch := context.WithCancel(ctx)
		cancels[i] = cancelWatch
		t.Cleanup(cancelWatch)
		watch := openQueueWatch(t, watchCtx, client, s.AddOrganization(name).GetOrganizationId())
		go func() {
			result := receipt{index: i}
			if watch.Receive() {
				result.event = watch.Msg()
				result.extra = watch.Receive()
			}
			result.err = watch.Err()
			results <- result
		}()
	}

	keepalive := queueKeepalive(0)
	s.SendQueueEvent(keepalive)
	first := <-results
	if !proto.Equal(first.event, keepalive) || first.extra || first.err != nil {
		t.Fatalf("watch %d got %v (extra message: %t, error: %v), want a keepalive and successful end", first.index, first.event, first.extra, first.err)
	}
	cancels[1-first.index]()
	second := <-results
	if second.event != nil || connect.CodeOf(second.err) != connect.CodeCanceled {
		t.Fatalf("watch %d got %v (error: %v), want only cancellation", second.index, second.event, second.err)
	}
}

func TestWatchQueueShutdownDiscardsUnscopedEvents(t *testing.T) {
	t.Parallel()

	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	update := queueKeepalive(0)
	s.SendQueueEvent(update)

	start := make(chan struct{})
	enqueued, closed := make(chan struct{}), make(chan struct{})
	sendBurst := func(done chan struct{}) {
		defer close(done)
		for range 65 {
			s.SendQueueEvent(update)
		}
	}
	go func() {
		<-start
		sendBurst(enqueued)
	}()
	go func() {
		<-start
		s.Close()
		close(closed)
	}()
	close(start)
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("shutdown blocked with queued unscoped events")
	}
	select {
	case <-enqueued:
	case <-ctx.Done():
		t.Fatal("unscoped sends blocked during shutdown")
	}

	late := make(chan struct{})
	go sendBurst(late)
	select {
	case <-late:
	case <-ctx.Done():
		t.Fatal("unscoped sends blocked after shutdown")
	}
	if len(s.unscopedQueue.events) != 0 {
		t.Fatal("unscoped events remained after shutdown")
	}
}

func TestWatchQueueExitsWhenCanceledWhileWaiting(t *testing.T) {
	t.Parallel()

	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	t.Cleanup(cancelWatch)
	org := s.AddOrganization("waiting").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	watch := openQueueWatch(t, watchCtx, client, org)

	cancelWatch()
	if watch.Receive() || connect.CodeOf(watch.Err()) != connect.CodeCanceled {
		t.Fatalf("got %v (error: %v), want canceled stream", watch.Msg(), watch.Err())
	}
}

func TestWatchQueueShutdownDiscardsScopedUpdates(t *testing.T) {
	t.Parallel()

	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	pendingOrg := s.AddOrganization("pending").GetOrganizationId()
	watchingOrg := s.AddOrganization("waiting").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	watch := openQueueWatch(t, ctx, client, watchingOrg)

	update := queueRunner(pendingOrg, "pending")
	s.SendQueueEvent(update)
	closed, enqueued := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(enqueued)
		for range 65 {
			s.SendQueueEvent(update)
		}
	}()
	go func() {
		defer close(closed)
		s.Close()
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("shutdown blocked with a waiting watch")
	}
	select {
	case <-enqueued:
	case <-ctx.Done():
		t.Fatal("scoped sends blocked during shutdown")
	}
	if watch.Receive() || ctx.Err() != nil {
		t.Fatalf("got %v (error: %v), want watch to end before its deadline", watch.Msg(), watch.Err())
	}

	late := make(chan struct{})
	go func() {
		defer close(late)
		for range 65 {
			s.SendQueueEvent(update)
		}
	}()
	select {
	case <-late:
	case <-ctx.Done():
		t.Fatal("scoped sends blocked after shutdown")
	}
	if len(s.queueByOrg) != 0 {
		t.Fatal("scoped queues remained after shutdown")
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
	otherOrg := s.AddOrganization("other").GetOrganizationId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	firstWatch := openQueueWatch(t, ctx, client, org)
	secondWatch := openQueueWatch(t, ctx, client, org)
	otherWatch := openQueueWatch(t, ctx, client, otherOrg)

	request := queueRequest(org, "request")
	runner := queueRunner(org, "runner")
	otherRequest := queueRequest(otherOrg, "other-request")
	s.SendQueueEvent(request)
	s.SendQueueEvent(runner)
	s.SendQueueEvent(otherRequest)

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
	requireQueueUpdate(t, otherWatch, otherRequest)
	requireQueueEOF(t, otherWatch)
}

func TestWatchQueueSnapshotProjectsDevSessionRequests(t *testing.T) {
	t.Parallel()
	s := New()
	t.Cleanup(s.Close)
	s.NewSessionState = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	_, ds := createExecTestSession(t, ctx, s, t.TempDir())
	_, other := createExecTestSession(t, ctx, s, t.TempDir())
	org, id := ds.GetOrganizationId(), ds.GetDevSessionId()
	client := sandrunv1connect.NewWorkServiceClient(http.DefaultClient, s.URL)
	seed := &sandrunv1.Request{OrganizationId: org, RequestId: "standalone", State: ds.GetState()}
	s.AddRequest(seed)
	s.AddRequest(&sandrunv1.Request{OrganizationId: org, RequestId: id, State: ds.GetState()})

	watch, err := client.WatchQueue(ctx, listTestRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watch.Close() })
	seen := make(map[string]*sandrunv1.Request)
	for watch.Receive() && watch.Msg().GetSnapshotComplete() == nil {
		r := watch.Msg().GetRequest()
		if r == nil || seen[r.GetRequestId()] != nil {
			t.Fatalf("unexpected snapshot event %v", watch.Msg())
		}
		seen[r.GetRequestId()] = r
	}
	if watch.Msg().GetSnapshotComplete() == nil || len(seen) != 2 || seen[other.GetDevSessionId()] != nil ||
		!proto.Equal(seen[seed.GetRequestId()], seed) {
		t.Fatalf("unexpected snapshot %v, last event %v, error %v", seen, watch.Msg(), watch.Err())
	}
	requireDevSessionWorkRequest(t, seen[id], ds, sandrunv1.Priority_PRIORITY_NORMAL)

	_, err = client.CancelRequest(ctx, listTestRequest(&sandrunv1.CancelRequestRequest{OrganizationId: org, RequestId: id}))
	if err != nil {
		t.Fatal(err)
	}
	update := queueRunner(org, "live")
	s.SendQueueEvent(update)
	requireQueueUpdate(t, watch, update)

	reopened, err := client.WatchQueue(ctx, listTestRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	requireQueueUpdate(t, reopened, &sandrunv1.WatchQueueResponse{Event: &sandrunv1.WatchQueueResponse_Request{Request: seed}})
	if !reopened.Receive() || reopened.Msg().GetSnapshotComplete() == nil {
		t.Fatalf("got %v (error: %v), want snapshot marker after only the standalone request", reopened.Msg(), reopened.Err())
	}
}
