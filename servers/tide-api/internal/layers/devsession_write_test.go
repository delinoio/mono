// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

type gatedWriteProvider struct {
	*ownsandfake.Fake
	started, resume         chan struct{}
	opens, chunks, finishes atomic.Int32
}

func (p *gatedWriteProvider) New(orgID, key string) (ownsandclient.Client, error) {
	client, err := p.Fake.New(orgID, key)
	if err != nil {
		return nil, err
	}
	return &gatedWriteClient{Client: client, provider: p}, nil
}

type gatedWriteClient struct {
	ownsandclient.Client
	provider *gatedWriteProvider
}

func (c *gatedWriteClient) OpenFileWrite(sandboxID string, params ownsand.FileWriteParams) (ownsandclient.FileWrite, error) {
	c.provider.opens.Add(1)
	write, err := c.Client.OpenFileWrite(sandboxID, params)
	if err != nil {
		return nil, err
	}
	return &gatedFileWrite{FileWrite: write, provider: c.provider}, nil
}

type gatedFileWrite struct {
	ownsandclient.FileWrite
	provider *gatedWriteProvider
}

func (w *gatedFileWrite) WriteChunk(ctx context.Context, offset int64, data []byte) (*ownsandv1.WriteFileResponse, error) {
	if w.provider.chunks.Add(1) == 1 {
		close(w.provider.started)
		select {
		case <-w.provider.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return w.FileWrite.WriteChunk(ctx, offset, data)
}

func (w *gatedFileWrite) Finish(ctx context.Context, offset int64, data []byte, size int64, mode uint32) (*ownsandv1.WriteFileResponse, error) {
	w.provider.finishes.Add(1)
	return w.FileWrite.Finish(ctx, offset, data, size, mode)
}

func TestDevSessionFileWriteWaitHonorsContext(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final bool
		code  connect.Code
	}{
		{name: "canceled chunk", code: connect.CodeCanceled},
		{name: "deadline on finish", final: true, code: connect.CodeDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &gatedWriteProvider{
				Fake: ownsandfake.New(), started: make(chan struct{}), resume: make(chan struct{}),
			}
			w := newWorkWorldWithProvider(t, devsession.Timing{}, provider.Fake, provider)
			s, sandboxID := w.runningSession(t)

			handler := w.env.App.Handler()
			httpClient, baseURL := startDevSessionH2C(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				if req.Header.Get("X-Test-Write-Waiter") == "true" {
					// End the server context first so the test observes the
					// handler's RPC response while the client still waits.
					const waiterLimit = 200 * time.Millisecond
					if tc.code == connect.CodeCanceled {
						ctx, cancel := context.WithCancel(req.Context())
						timer := time.AfterFunc(waiterLimit, cancel)
						defer timer.Stop()
						defer cancel()
						req = req.WithContext(ctx)
					} else {
						ctx, cancel := context.WithTimeout(req.Context(), waiterLimit)
						defer cancel()
						req = req.WithContext(ctx)
					}
				}
				handler.ServeHTTP(rw, req)
			}), 1<<16)

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resume := sync.OnceFunc(func() { close(provider.resume) })
			defer resume()
			client := tidev1connect.NewDevSessionServiceClient(httpClient, baseURL)
			writeID := api.NewID()
			write := func(ctx context.Context, offset int64, data string, final, waiter bool) (*connect.Response[tidev1.WriteDevSessionFileResponse], error) {
				req := connect.NewRequest(&tidev1.WriteDevSessionFileRequest{
					OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), WriteId: writeID,
					Path: "fixture.txt", Offset: offset, Data: []byte(data), Final: final,
				})
				req.Header().Set("Authorization", "Bearer "+testenv.UserTokenPrefix+w.org.Member)
				if waiter {
					req.Header().Set("X-Test-Write-Waiter", "true")
				}
				return client.WriteDevSessionFile(ctx, req)
			}
			first := make(chan error, 1)
			go func() {
				_, err := write(t.Context(), 0, "a", false, false)
				first <- err
			}()
			select {
			case <-provider.started:
			case err := <-first:
				t.Fatalf("first chunk returned before the provider gate: %v", err)
			case <-ctx.Done():
				t.Fatal("first chunk did not reach OwnSand")
			}

			_, err := write(ctx, 1, "b", tc.final, true)
			wantErr := context.Canceled
			if tc.code == connect.CodeDeadlineExceeded {
				wantErr = context.DeadlineExceeded
			}
			var rpcErr *connect.Error
			if ctx.Err() != nil || !errors.As(err, &rpcErr) || rpcErr.Code() != tc.code || rpcErr.Message() != wantErr.Error() {
				t.Fatalf("waiter returned %v with client context %v, want server response %v while the first chunk is blocked", err, ctx.Err(), tc.code)
			}
			if chunks, finishes := provider.chunks.Load(), provider.finishes.Load(); chunks != 1 || finishes != 0 {
				t.Fatalf("provider calls before unblocking first chunk: chunks=%d finishes=%d", chunks, finishes)
			}
			t.Logf("Tide returned %s over HTTP/2 before the first chunk was released; OwnSand saw one chunk and zero finishes", tc.code)

			resume()
			select {
			case err := <-first:
				if err != nil {
					t.Fatalf("first chunk: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("first chunk did not complete")
			}
			r, err := write(ctx, 1, "b", true, false)
			if err != nil || !r.Msg.GetFinished() || r.Msg.GetSizeBytes() != 2 {
				t.Fatalf("retry: %v, %v", r, err)
			}
			if opens, chunks, finishes := provider.opens.Load(), provider.chunks.Load(), provider.finishes.Load(); opens != 1 || chunks != 1 || finishes != 1 {
				t.Fatalf("provider calls after retry: opens=%d chunks=%d finishes=%d", opens, chunks, finishes)
			}
			if got, ok := w.fake.File(w.osOrg, sandboxID, "/home/dev/work/fixture.txt"); !ok || string(got) != "ab" {
				t.Fatalf("unexpected file contents: %q (exists=%t)", got, ok)
			}
			t.Log("The first chunk succeeded; the live final retry reused the one open handle and stored both fixture bytes")
		})
	}
}
