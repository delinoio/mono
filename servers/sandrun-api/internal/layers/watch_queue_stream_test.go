// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

type watchFlushObserver struct {
	http.ResponseWriter
	beforeFlush func()
}

func (w watchFlushObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w watchFlushObserver) Flush() {
	w.beforeFlush()
	w.ResponseWriter.(http.Flusher).Flush()
}

func TestWatchQueueStalledResponse(t *testing.T) {
	const procedure = sandrunv1connect.WorkServiceWatchQueueProcedure
	snapshotComplete := &sandrunv1.WatchQueueResponse{
		Event: &sandrunv1.WatchQueueResponse_SnapshotComplete{SnapshotComplete: &sandrunv1.WatchQueueSnapshotComplete{}},
	}
	snapshotWindow := 5 + proto.Size(snapshotComplete)
	for _, tc := range []struct {
		name      string
		window    int
		keepalive time.Duration
	}{
		{"snapshot", 1, time.Second},
		{"delta", snapshotWindow, time.Second},
		{"keepalive", snapshotWindow, 50 * time.Millisecond},
		{"final status", snapshotWindow, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timing := work.WatchQueueTiming{
				Lifetime: 600 * time.Millisecond, WriteGrace: 250 * time.Millisecond,
				Poll: 20 * time.Millisecond, Keepalive: tc.keepalive,
			}
			env := testenv.New(t, testenv.WithLayerOptions(layers.Options{
				OwnSand: ownsandfake.New(), WatchQueueTiming: timing,
			}, &layers.Layers{}))
			org := env.NewOrg("alice")
			if tc.name == "snapshot" {
				seedQueueWatchRow(t, env, org.ID, false)
			}
			type responseTiming struct {
				elapsed time.Duration
				flushes []time.Duration
			}
			finished := make(chan responseTiming, 1)
			handler := env.App.Handler()
			client, baseURL := startStreamH2C(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := time.Now()
				var flushes []time.Duration
				handler.ServeHTTP(watchFlushObserver{w, func() {
					flushes = append(flushes, time.Since(start))
				}}, r)
				finished <- responseTiming{time.Since(start), flushes}
			}), tc.window)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			msg, err := proto.Marshal(&sandrunv1.WatchQueueRequest{OrganizationId: org.ID})
			if err != nil {
				t.Fatal(err)
			}
			body := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(msg)))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+procedure, bytes.NewReader(append(body, msg...)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/connect+proto")
			req.Header.Set("Authorization", "Bearer "+testenv.UserTokenPrefix+org.Member)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
				t.Fatalf("response = %s %s, want HTTP/2 200", resp.Proto, resp.Status)
			}
			if tc.name == "delta" {
				seedQueueWatchRow(t, env, org.ID, false)
			}
			// Leave the body unread so the peer exhausts the stream window.
			select {
			case result := <-finished:
				deadline := timing.Lifetime + timing.WriteGrace
				if result.elapsed < deadline || result.elapsed > deadline+time.Second {
					t.Fatalf("stalled handler returned after %s, want write deadline near %s", result.elapsed, deadline)
				}
				// The snapshot case blocks the first flush. The remaining
				// cases fill the window with snapshot_complete first.
				flush := 1
				if tc.name == "snapshot" {
					flush = 0
				}
				if flush >= len(result.flushes) || (result.flushes[flush] >= timing.Lifetime) != (tc.name == "final status") {
					t.Fatalf("unexpected %s flush times: %v", tc.name, result.flushes)
				}
				t.Logf("HTTP/2 handler released the stalled %s after %s (flush began at %s)", tc.name, result.elapsed, result.flushes[flush])
			case <-ctx.Done():
				t.Fatal("stalled HTTP/2 handler did not return by the write deadline")
			}
			if _, err := io.Copy(io.Discard, resp.Body); err == nil || ctx.Err() != nil {
				t.Fatalf("body read = %v with context %v, want a transport error from the server deadline", err, ctx.Err())
			}
		})
	}
}

func TestWatchQueueRotationAndReconnect(t *testing.T) {
	timing := work.WatchQueueTiming{
		Lifetime: 600 * time.Millisecond, WriteGrace: 250 * time.Millisecond,
		Poll: 20 * time.Millisecond, Keepalive: 100 * time.Millisecond,
	}
	env := testenv.New(t, testenv.WithLayerOptions(layers.Options{
		OwnSand: ownsandfake.New(), WatchQueueTiming: timing,
	}, &layers.Layers{}))
	org := env.NewOrg("alice")
	requestID := seedQueueWatchRow(t, env, org.ID, false)
	runnerID := seedQueueWatchRow(t, env, org.ID, true)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()

	readSnapshot := func(ids ...string) *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse] {
		t.Helper()
		watch := startQueueWatch(t, ctx, env, org)
		t.Cleanup(func() { _ = watch.Close() })
		for _, id := range ids {
			expectQueueWatchState(t, nextQueueWatchEvent(t, watch), id, watchRequestOpen, watchRunnerOpen)
		}
		if event := nextQueueWatchEvent(t, watch); event.GetSnapshotComplete() == nil {
			t.Fatalf("expected snapshot_complete, got %v", event)
		}
		return watch
	}
	readToRotation := func(watch *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse]) {
		t.Helper()
		keepalives := 0
		for watch.Receive() {
			if watch.Msg().GetKeepalive() == nil {
				t.Fatalf("expected keepalive, got %v", watch.Msg())
			}
			keepalives++
		}
		if err := watch.Err(); err != nil || keepalives == 0 {
			t.Fatalf("rotation = %v after %d keepalives, want OK after at least one keepalive", err, keepalives)
		}
		t.Logf("Connect received %d keepalives and rotated with OK", keepalives)
	}

	watch := readSnapshot(requestID, runnerID)
	deltaID := seedQueueWatchRow(t, env, org.ID, false)
	delta := nextQueueWatchEvent(t, watch)
	for delta.GetKeepalive() != nil {
		delta = nextQueueWatchEvent(t, watch)
	}
	expectQueueWatchState(t, delta, deltaID, watchRequestOpen, watchRunnerOpen)
	readToRotation(watch)
	readToRotation(readSnapshot(requestID, runnerID, deltaID))
}
