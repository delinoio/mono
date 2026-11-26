// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type transferHandlers struct {
	write func(context.Context, *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error)
	read  func(context.Context, *connect.Request[sandrunv1.ReadDevSessionFileRequest], *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error
}

func transferTestClient(t *testing.T, upstream string, h transferHandlers) *Client {
	t.Helper()
	mux := http.NewServeMux()
	if upstream != "" {
		u, err := url.Parse(upstream)
		if err != nil {
			t.Fatal(err)
		}
		mux.Handle("/", httputil.NewSingleHostReverseProxy(u))
	}
	if h.write != nil {
		path := sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure
		mux.Handle(path, connect.NewUnaryHandler(path, h.write))
	}
	if h.read != nil {
		path := sandrunv1connect.DevSessionServiceReadDevSessionFileProcedure
		mux.Handle(path, connect.NewServerStreamHandler(path, h.read))
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, APIKey: "test-access-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.timing.backoffMin = 5 * time.Millisecond
	c.timing.backoffMax = 20 * time.Millisecond
	return c
}

func transferTestChunk(stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse], offset int64, data string) error {
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_Chunk{
		Chunk: &sandrunv1.DevSessionFileChunk{Offset: offset, Data: []byte(data)},
	}})
}

func transferTestEnd(stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse], size int64) error {
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_End{
		End: &sandrunv1.DevSessionFileEnd{SizeBytes: size},
	}})
}

func requireTransferDeadline(t *testing.T, caller context.Context, err error) {
	t.Helper()
	if caller.Err() != nil || !errors.Is(err, errTransferBudget) || connect.CodeOf(err) != connect.CodeDeadlineExceeded || connect.IsWireError(err) {
		t.Fatalf("transfer error = %v, caller error = %v, want local SDK deadline", err, caller.Err())
	}
}

func TestTransferWriteDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backoff bool
		caller  error
	}{
		{name: "stalled RPC"},
		{name: "oversized backoff", backoff: true},
		{name: "earlier caller deadline", caller: context.DeadlineExceeded},
		{name: "caller deadline during backoff", caller: context.DeadlineExceeded, backoff: true},
		{name: "caller cancellation during RPC", caller: context.Canceled},
		{name: "caller cancellation during backoff", caller: context.Canceled, backoff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{})
			c := transferTestClient(t, "", transferHandlers{write: func(ctx context.Context, _ *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				if tc.backoff {
					return nil, connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable"))
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}})
			c.timing.retryBudget = 200 * time.Millisecond
			if tc.backoff {
				c.timing.backoffMin = 5 * time.Second
				c.timing.backoffMax = 5 * time.Second
			}
			guard, stop := context.WithTimeout(t.Context(), 4*time.Second)
			defer stop()
			ctx := guard
			if tc.caller != nil {
				c.timing.retryBudget = 3 * time.Second
				if tc.caller == context.DeadlineExceeded {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
					defer cancel()
				} else {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					go func() {
						select {
						case <-started:
							if tc.backoff {
								_ = sleep(guard, 100*time.Millisecond)
							}
							cancel()
						case <-guard.Done():
						}
					}()
				}
			}
			n, err := c.writeGuestFile(ctx, &sandrunv1.DevSession{}, "/test", 0o600, strings.NewReader("content"), nil)
			if tc.caller == nil {
				requireTransferDeadline(t, ctx, err)
			} else if err != tc.caller {
				t.Errorf("error = %v, want caller's %v", err, tc.caller)
			}
			if n != 0 || calls.Load() != 1 {
				t.Errorf("wrote %d bytes in %d requests, want 0 bytes and 1 request", n, calls.Load())
			}
		})
	}
}

func TestTransferWriteResumeAndNextChunkBudget(t *testing.T) {
	const budget = 650 * time.Millisecond
	var calls atomic.Int32
	requests := make(chan *sandrunv1.WriteDevSessionFileRequest, 3)
	c := transferTestClient(t, "", transferHandlers{write: func(ctx context.Context, req *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		attempt := calls.Add(1)
		if attempt > 3 {
			return nil, connect.NewError(connect.CodeInternal, errors.New("unexpected call"))
		}
		requests <- req.Msg
		if attempt == 1 {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable"))
		}
		if err := sleep(ctx, 375*time.Millisecond); err != nil {
			return nil, err
		}
		return connect.NewResponse(&sandrunv1.WriteDevSessionFileResponse{}), nil
	}})
	c.timing.retryBudget = budget
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	start := time.Now()
	data := strings.Repeat("a", ChunkSize) + "z"
	n, err := c.writeGuestFile(ctx, &sandrunv1.DevSession{}, "/test", 0o640, strings.NewReader(data), nil)
	if err != nil || n != int64(len(data)) || calls.Load() != 3 || time.Since(start) <= budget {
		t.Fatalf("wrote %d bytes in %d requests: %v", n, calls.Load(), err)
	}
	first, retry, last := <-requests, <-requests, <-requests
	if first.GetWriteId() == "" || !proto.Equal(first, retry) || first.GetOffset() != 0 || first.GetFinal() || string(first.GetData()) != data[:ChunkSize] {
		t.Fatal("first write and retry differ from the expected first chunk")
	}
	if last.GetWriteId() != first.GetWriteId() || last.GetOffset() != ChunkSize || !last.GetFinal() || last.GetMode() != 0o640 || string(last.GetData()) != "z" {
		t.Fatal("last write did not finalize the same upload at the next offset")
	}
}

func TestTransferReadDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		empty   bool
		backoff bool
	}{
		{name: "before first message"},
		{name: "stalled after progress", partial: true},
		{name: "empty chunks after progress", partial: true, empty: true},
		{name: "oversized backoff", backoff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, emptyChunks atomic.Int32
			c := transferTestClient(t, "", transferHandlers{read: func(ctx context.Context, _ *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
				calls.Add(1)
				if tc.backoff {
					return connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable"))
				}
				if tc.partial {
					if err := transferTestChunk(stream, 0, "a"); err != nil {
						return err
					}
				}
				if tc.empty {
					for sleep(ctx, 40*time.Millisecond) == nil {
						if err := transferTestChunk(stream, 1, ""); err != nil {
							return err
						}
						emptyChunks.Add(1)
					}
				}
				<-ctx.Done()
				return ctx.Err()
			}})
			c.timing.retryBudget = 250 * time.Millisecond
			if tc.backoff {
				c.timing.backoffMin = 5 * time.Second
				c.timing.backoffMax = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			var out bytes.Buffer
			n, err := c.readGuestFile(ctx, &sandrunv1.DevSession{}, "/test", 9, &out, nil)
			requireTransferDeadline(t, ctx, err)
			want := ""
			if tc.partial {
				want = "a"
			}
			if n != int64(len(want)) || out.String() != want || calls.Load() != 1 || tc.empty && emptyChunks.Load() == 0 {
				t.Errorf("read %q (%d bytes), calls = %d, empty chunks sent = %d", out.String(), n, calls.Load(), emptyChunks.Load())
			}
		})
	}
}

func TestTransferReadProgressRenewsIdleBudget(t *testing.T) {
	const budget = 600 * time.Millisecond
	c := transferTestClient(t, "", transferHandlers{read: func(ctx context.Context, _ *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
		if err := transferTestChunk(stream, 0, "a"); err != nil {
			return err
		}
		for i, data := range []string{"b", "c"} {
			if err := sleep(ctx, 350*time.Millisecond); err != nil {
				return err
			}
			if err := transferTestChunk(stream, int64(i+1), data); err != nil {
				return err
			}
		}
		return transferTestEnd(stream, 3)
	}})
	c.timing.retryBudget = budget
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	n, err := c.readGuestFile(ctx, &sandrunv1.DevSession{}, "/test", 9, &out, nil)
	if err != nil || n != 3 || out.String() != "abc" || time.Since(start) <= budget {
		t.Fatalf("read %q (%d bytes): %v", out.String(), n, err)
	}
}

func TestTransferReadReconnectKeepsIdleBudget(t *testing.T) {
	var calls atomic.Int32
	var readID atomic.Pointer[string]
	c := transferTestClient(t, "", transferHandlers{read: func(ctx context.Context, req *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
		if calls.Add(1) == 1 {
			id := req.Msg.GetReadId()
			readID.Store(&id)
			if err := transferTestChunk(stream, 0, "a"); err != nil {
				return err
			}
			if err := sleep(ctx, 350*time.Millisecond); err != nil {
				return err
			}
			return connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable"))
		}
		if req.Msg.GetReadId() != *readID.Load() || req.Msg.GetOffset() != 1 {
			return connect.NewError(connect.CodeInternal, errors.New("unexpected read continuation"))
		}
		if err := sleep(ctx, 350*time.Millisecond); err != nil {
			return err
		}
		return transferTestEnd(stream, 1)
	}})
	c.timing.retryBudget = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var out bytes.Buffer
	n, err := c.readGuestFile(ctx, &sandrunv1.DevSession{}, "/test", 9, &out, nil)
	requireTransferDeadline(t, ctx, err)
	if n != 1 || out.String() != "a" || calls.Load() != 2 {
		t.Fatalf("read %q (%d bytes) in %d requests", out.String(), n, calls.Load())
	}
}

func TestTransferReadEndAndTerminalError(t *testing.T) {
	for _, code := range []connect.Code{0, connect.CodePermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			var calls atomic.Int32
			c := transferTestClient(t, "", transferHandlers{read: func(_ context.Context, _ *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
				calls.Add(1)
				if code != 0 {
					return connect.NewError(code, errors.New("fixture denied"))
				}
				if err := transferTestChunk(stream, 0, ""); err != nil {
					return err
				}
				return transferTestEnd(stream, 0)
			}})
			n, err := c.readGuestFile(t.Context(), &sandrunv1.DevSession{}, "/test", 9, &bytes.Buffer{}, nil)
			if n != 0 || calls.Load() != 1 {
				t.Fatalf("read %d bytes in %d requests: %v", n, calls.Load(), err)
			}
			if code == 0 {
				if err != nil {
					t.Fatalf("empty file: %v", err)
				}
			} else if connect.CodeOf(err) != code || !connect.IsWireError(err) {
				t.Fatalf("error = %v, want wire %v", err, code)
			}
		})
	}
}

func TestSyncUploadDeadlineReleasesLock(t *testing.T) {
	needPOSIX(t)
	setup, srv, org := newTestClient(t, "")
	ds := newSession(t, setup, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	local := newRepo(t, map[string]string{"fixture.txt": "new content"}, nil, "")
	lock := ds.GetWorkspacePath() + lockSuffix
	var calls atomic.Int32
	c := transferTestClient(t, srv.URL, transferHandlers{write: func(ctx context.Context, req *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		calls.Add(1)
		if !strings.HasSuffix(req.Msg.GetPath(), ".tar") {
			return nil, fmt.Errorf("unexpected upload path %q", req.Msg.GetPath())
		}
		if _, err := os.Stat(lock); err != nil {
			return nil, fmt.Errorf("upload began without a guest lock: %w", err)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	c.timing.retryBudget = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: local})
	requireTransferDeadline(t, ctx, err)
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) || calls.Load() != 1 {
		t.Errorf("stat of lock after stalled upload = %v, upload attempts = %d", err, calls.Load())
	}
}

func TestDownloadReadDeadlineKeepsDestination(t *testing.T) {
	needPOSIX(t)
	setup, srv, org := newTestClient(t, "")
	ds := newSession(t, setup, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	if err := os.WriteFile(filepath.Join(ds.GetWorkspacePath(), "fixture.txt"), []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(dest, []byte("previous content"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := transferTestClient(t, srv.URL, transferHandlers{read: func(ctx context.Context, _ *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
		if err := transferTestChunk(stream, 0, "new"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	c.timing.retryBudget = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "fixture.txt", LocalPath: dest})
	requireTransferDeadline(t, ctx, err)
	if got, err := os.ReadFile(dest); err != nil || string(got) != "previous content" {
		t.Fatalf("destination after stalled read = %q, %v", got, err)
	}
}
