// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

type gatedWriteProvider struct {
	*ownsandfake.Fake
	started, resume  chan struct{}
	chunks, finishes atomic.Int32
	mu               sync.Mutex
	opens            []observedWriteOpen
}

type observedWriteOpen struct {
	key, sandboxID string
	params         ownsand.FileWriteParams
}

func (p *gatedWriteProvider) writeOpens() []observedWriteOpen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.opens)
}

func (p *gatedWriteProvider) New(orgID, key string) (ownsandclient.Client, error) {
	client, err := p.Fake.New(orgID, key)
	if err != nil {
		return nil, err
	}
	return &gatedWriteClient{Client: client, provider: p, key: key}, nil
}

type gatedWriteClient struct {
	ownsandclient.Client
	provider *gatedWriteProvider
	key      string
}

func (c *gatedWriteClient) OpenFileWrite(sandboxID string, params ownsand.FileWriteParams) (ownsandclient.FileWrite, error) {
	c.provider.mu.Lock()
	c.provider.opens = append(c.provider.opens, observedWriteOpen{key: c.key, sandboxID: sandboxID, params: params})
	c.provider.mu.Unlock()
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
	first := w.provider.chunks.Add(1) == 1
	res, err := w.FileWrite.WriteChunk(ctx, offset, data)
	if !first || err != nil {
		return res, err
	}
	// Hold the response after the guest retains the chunk. The credential
	// can change while SandRun still owns the write entry.
	close(w.provider.started)
	select {
	case <-w.provider.resume:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *gatedFileWrite) Finish(ctx context.Context, offset int64, data []byte, size int64, mode uint32) (*ownsandv1.WriteFileResponse, error) {
	w.provider.finishes.Add(1)
	return w.FileWrite.Finish(ctx, offset, data, size, mode)
}

func TestDevSessionFileWriteWaitHonorsContext(t *testing.T) {
	t.Parallel()
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
			httpClient, baseURL := startStreamH2C(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
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
			client := sandrunv1connect.NewDevSessionServiceClient(httpClient, baseURL)
			writeID := api.NewID()
			write := func(ctx context.Context, offset int64, data string, final, waiter bool) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
				req := connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
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
			t.Logf("SandRun returned %s over HTTP/2 before the first chunk was released; OwnSand saw one chunk and zero finishes", tc.code)

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
			if opens, chunks, finishes := len(provider.writeOpens()), provider.chunks.Load(), provider.finishes.Load(); opens != 1 || chunks != 1 || finishes != 1 {
				t.Fatalf("provider calls after retry: opens=%d chunks=%d finishes=%d", opens, chunks, finishes)
			}
			if got, ok := w.fake.File(w.osOrg, sandboxID, "/home/dev/work/fixture.txt"); !ok || string(got) != "ab" {
				t.Fatalf("unexpected file contents: %q (exists=%t)", got, ok)
			}
			t.Log("The first chunk succeeded; the live final retry reused the one open handle and stored both fixture bytes")
		})
	}
}

func TestDevSessionFileWriteRefreshesCredential(t *testing.T) {
	t.Parallel()
	provider := &gatedWriteProvider{
		Fake: ownsandfake.New(), started: make(chan struct{}), resume: make(chan struct{}),
	}
	w := newWorkWorldWithProvider(t, devsession.Timing{}, provider.Fake, provider)
	session, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resume := sync.OnceFunc(func() { close(provider.resume) })
	defer resume()

	waiterStarted := make(chan struct{})
	handler := w.env.App.Handler()
	httpClient, baseURL := startStreamH2C(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		close(waiterStarted)
		handler.ServeHTTP(rw, req)
	}), 1<<16)
	waiter := sandrunv1connect.NewDevSessionServiceClient(httpClient, baseURL)

	const path = "rotation.txt"
	writeID, expiredID := api.NewID(), api.NewID()
	write := func(client sandrunv1connect.DevSessionServiceClient, id, path string, offset int64, data string, final bool) (*sandrunv1.WriteDevSessionFileResponse, error) {
		req := connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), WriteId: id, Path: path,
			Offset: offset, Data: []byte(data), Final: final,
		})
		req.Header().Set("Authorization", "Bearer "+testenv.UserTokenPrefix+w.org.Member)
		res, err := client.WriteDevSessionFile(ctx, req)
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	first := make(chan error, 1)
	go func() {
		_, err := write(owner.DevSession, writeID, path, 0, "a", false)
		first <- err
	}()
	select {
	case <-provider.started:
	case err := <-first:
		t.Fatalf("first chunk returned before the gate: %v", err)
	case <-ctx.Done():
		t.Fatal("first chunk did not reach OwnSand")
	}

	// This different write can reach OwnSand while the first response is held.
	if _, err := write(owner.DevSession, expiredID, "expired.txt", 0, "x", false); err != nil {
		t.Fatalf("first chunk of the second write: %v", err)
	}

	waitingResult := make(chan error, 1)
	go func() {
		_, err := write(waiter, writeID, path, 1, "b", false)
		waitingResult <- err
	}()
	select {
	case <-waiterStarted:
	case <-ctx.Done():
		t.Fatal("waiting request did not reach SandRun")
	}
	select {
	case err := <-waitingResult:
		t.Fatalf("waiting chunk returned while the first response was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// AddOrg replaces the key for this fake organization and rejects further
	// calls with the old key. It keeps the sandboxes and their write state.
	newKey := ownsandKey('b')
	w.fake.AddOrg(w.osOrg, newKey)
	if _, err := w.setLink(w.org.Admin, w.osOrg, newKey); err != nil {
		t.Fatal(err)
	}
	if got := provider.chunks.Load(); got != 2 {
		t.Fatalf("%d chunks reached OwnSand while the first response was held, want 2", got)
	}
	resume()
	for _, result := range []<-chan error{first, waitingResult} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("waiting for the chunks to complete")
		}
	}

	_, err := write(owner.DevSession, writeID, "wrong.txt", 2, "c", true)
	wantCode(t, "path change after rotation", err, connect.CodeInvalidArgument)
	res, err := write(owner.DevSession, writeID, path, 2, "c", true)
	if err != nil || res.GetOffset() != 2 || res.GetLength() != 1 || !res.GetFinished() || res.GetSizeBytes() != 3 {
		t.Fatalf("finish: %v, %v", res, err)
	}
	const resolved = "/home/dev/work/" + path
	if got, ok := w.fake.File(w.osOrg, sandboxID, resolved); !ok || string(got) != "abc" {
		t.Fatalf("file = %q (exists=%t), want abc", got, ok)
	}
	opens := provider.writeOpens()
	if len(opens) != 3 {
		t.Fatalf("opened %d handles, want one per write and one after rotation", len(opens))
	}
	before, after := opens[0], opens[2]
	if before.key != ownsandKey('a') || after.key != newKey ||
		before.sandboxID != sandboxID || after.sandboxID != sandboxID ||
		after.params.WriteID != writeID || after.params.Path != resolved || len(after.params.BindingSalt) == 0 ||
		before.params.WriteID != after.params.WriteID || before.params.Path != after.params.Path ||
		!slices.Equal(before.params.BindingSalt, after.params.BindingSalt) {
		t.Fatal("the rotated handle did not use the replacement key and the original write binding")
	}

	if _, err := write(owner.DevSession, writeID, path, 2, "c", true); err != nil {
		t.Fatalf("replay finished write: %v", err)
	}
	if n := len(provider.writeOpens()); n != 4 {
		t.Fatalf("finish replay opened %d total handles, want 4 after eviction", n)
	}

	// Make the fake guest report that its retained write expired.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.FileError = connect.NewWireError(connect.CodeNotFound, errors.New("write expired"))
	})
	for range 2 {
		_, err := write(owner.DevSession, expiredID, "expired.txt", 1, "y", true)
		wantCode(t, "expired write", err, connect.CodeNotFound)
	}
	opens = provider.writeOpens()
	if len(opens) != 6 || opens[4].key != newKey || opens[5].key != newKey {
		t.Fatal("expected a fresh handle with the replacement key on each expired write attempt")
	}
}

func TestDevSessionReplaysLostWriteFinish(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	session, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	ctx := t.Context()
	const path = "fixture.txt"

	writeID := api.NewID()
	if _, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
		OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), WriteId: writeID,
		Path: path, Data: []byte("abc"),
	})); err != nil {
		t.Fatal(err)
	}

	// Send the final request over HTTP but drop its response before Connect
	// can deliver it to the caller.
	client := sandrunv1connect.NewDevSessionServiceClient(dropWriteResponse{owner.HTTP}, w.env.Server.URL)
	final := &sandrunv1.WriteDevSessionFileRequest{
		OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), WriteId: writeID,
		Path: path, Offset: 3, Data: []byte("def"), Final: true, Mode: 0o600,
	}
	_, err := client.WriteDevSessionFile(ctx, connect.NewRequest(final))
	wantCode(t, "lost finish response", err, connect.CodeUnavailable)
	checkFile := func(want string) {
		t.Helper()
		got, ok := w.fake.File(w.osOrg, sandboxID, "/home/dev/work/"+path)
		if !ok || string(got) != want {
			t.Fatalf("file = %q (exists %t), want %q", got, ok, want)
		}
	}
	checkFile("abcdef")

	if _, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
		OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), WriteId: api.NewID(),
		Path: path, Data: []byte("newer"), Final: true, Mode: 0o644,
	})); err != nil {
		t.Fatal(err)
	}
	got, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(final))
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Msg; r.GetWriteId() != writeID || r.GetOffset() != 3 || r.GetLength() != 3 || !r.GetFinished() || r.GetSizeBytes() != 6 {
		t.Fatalf("replayed finish = %v, want write %s finished at offset 3 with length 3 and size 6", r, writeID)
	}
	checkFile("newer")
}

type dropWriteResponse struct{ client *http.Client }

func (d dropWriteResponse) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("expected a successful response before simulating its loss")
	}
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("finish response lost"))
}
