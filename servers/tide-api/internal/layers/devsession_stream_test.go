// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func startDevSessionH2C(t *testing.T, handler http.Handler, window int) (*http.Client, string) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = &protocols
	server.Start()
	t.Cleanup(server.Close)

	transport := &http.Transport{
		Protocols: &protocols,
		HTTP2:     &http.HTTP2Config{MaxReceiveBufferPerStream: window},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := transport.NewClientConn(ctx, "http", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// RoundTrip uses this one connection for every request, including a
	// concurrent request while the dev session response is blocked.
	return &http.Client{Transport: conn}, server.URL
}

func TestDevSessionStalledResponse(t *testing.T) {
	const relayLimit = 600 * time.Millisecond
	const writeGrace = 300 * time.Millisecond
	w := newWorkWorld(t, devsession.Timing{
		StreamLimit: relayLimit, WriteGracePeriod: writeGrace, KeepaliveInterval: 50 * time.Millisecond,
	})
	session, sandboxID := w.runningSession(t)
	orgID, sessionID, execID := w.org.ID, session.GetDevSessionId(), api.NewID()
	path := "download.txt"
	w.fake.SetFile(w.osOrg, sandboxID, "/home/dev/work/"+path, bytes.Repeat([]byte("x"), 2000))
	code := int64(7)
	final := &tidev1.ExecDevSessionResponse{Event: &tidev1.DevSessionExecEvent{
		Event: &tidev1.DevSessionExecEvent_Exited{Exited: &tidev1.DevSessionExecExited{
			Status: &tidev1.DevSessionExecExited_ExitCode{ExitCode: code},
		}},
	}}
	exec := &tidev1.ExecDevSessionRequest{
		OrganizationId: orgID, DevSessionId: sessionID, ExecId: execID, Command: []string{"test-command"},
	}
	for _, tc := range []struct {
		name, procedure string
		request         proto.Message
		window          int
		wantRelayExpiry bool
	}{
		{"exec output", tidev1connect.DevSessionServiceExecDevSessionProcedure, exec, 64, true},
		{"attach keepalive", tidev1connect.DevSessionServiceAttachDevSessionExecProcedure, &tidev1.AttachDevSessionExecRequest{
			OrganizationId: orgID, DevSessionId: sessionID, ExecId: execID,
		}, 1, true},
		{"file chunk", tidev1connect.DevSessionServiceReadDevSessionFileProcedure, &tidev1.ReadDevSessionFileRequest{
			OrganizationId: orgID, DevSessionId: sessionID, ReadId: api.NewID(), Path: path,
		}, 64, false},
		// The receive window fits the complete final exec event and no bytes
		// of the subsequent Connect end-of-stream envelope.
		{"terminal status", tidev1connect.DevSessionServiceExecDevSessionProcedure, exec, 5 + proto.Size(final), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expired := make(chan error, 1)
			finished := make(chan struct{})
			w.fake.SetExec(func(ctx context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
				if !tc.wantRelayExpiry {
					return &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
				}
				// Observe cancellation even when the output callback is blocked.
				context.AfterFunc(ctx, func() { expired <- ctx.Err() })
				if err := p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStdout, Data: bytes.Repeat([]byte("x"), 1024)}); err != nil {
					return nil, err
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}, func(ctx context.Context, _, _ string, _ ownsand.AttachParams) (*ownsand.ExecResult, error) {
				<-ctx.Done()
				expired <- ctx.Err()
				return nil, ctx.Err()
			})
			handler := w.env.App.Handler()
			client, baseURL := startDevSessionH2C(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.procedure {
					defer close(finished)
				}
				handler.ServeHTTP(w, r)
			}), tc.window)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			msg, err := proto.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			body := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(msg)))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+tc.procedure, bytes.NewReader(append(body, msg...)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/connect+proto")
			req.Header.Set("Authorization", "Bearer "+testenv.UserTokenPrefix+w.org.Member)
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
				t.Fatalf("response = %s %s, want HTTP/2 200", resp.Proto, resp.Status)
			}
			checkPeer := func() {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if _, err := io.Copy(io.Discard, resp.Body); err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("peer request = %s, body error = %v", resp.Status, err)
				}
			}
			checkPeer()
			if tc.wantRelayExpiry {
				select {
				case err := <-expired:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("upstream context = %v, want relay deadline", err)
					}
				case <-ctx.Done():
					t.Fatal("OwnSand relay did not expire")
				}
			}
			select {
			case <-finished:
				if elapsed := time.Since(start); elapsed < relayLimit+writeGrace {
					t.Fatalf("stalled handler returned after %s, before the write deadline", elapsed)
				}
			case <-ctx.Done():
				t.Fatal("stalled handler did not return by the write deadline")
			}
			if _, err := io.Copy(io.Discard, resp.Body); err == nil {
				t.Fatal("stalled response ended without a transport error")
			}
			checkPeer()
		})
	}
}

func TestDevSessionFileReadRotation(t *testing.T) {
	const relayLimit = 200 * time.Millisecond
	w := newWorkWorld(t, devsession.Timing{StreamLimit: relayLimit, WriteGracePeriod: 2 * time.Second})
	session, sandboxID := w.runningSession(t)
	httpClient, baseURL := startDevSessionH2C(t, w.env.App.Handler(), 64)
	client := tidev1connect.NewDevSessionServiceClient(httpClient, baseURL)
	path := "download.txt"
	data := make([]byte, 1500)
	rand.Read(data)
	w.fake.SetFile(w.osOrg, sandboxID, "/home/dev/work/"+path, data)
	readID := api.NewID()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	read := func(offset int64) *connect.ServerStreamForClient[tidev1.ReadDevSessionFileResponse] {
		t.Helper()
		req := connect.NewRequest(&tidev1.ReadDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), ReadId: readID, Path: path, Offset: offset,
		})
		req.Header().Set("Authorization", "Bearer "+testenv.UserTokenPrefix+w.org.Member)
		stream, err := client.ReadDevSessionFile(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stream.Close() })
		return stream
	}

	stream := read(0)
	// Wait for the first chunk to start filling the receive window. Keep it
	// blocked through relay rotation, then drain it during the write grace.
	_ = stream.ResponseHeader()
	time.Sleep(relayLimit)
	if !stream.Receive() {
		t.Fatalf("missing first chunk: %v", stream.Err())
	}
	chunk := stream.Msg().GetChunk()
	if chunk.GetOffset() != 0 || !bytes.Equal(chunk.GetData(), data[:1000]) {
		t.Fatal("first chunk did not match the start of the snapshot")
	}
	if stream.Receive() || stream.Err() != nil {
		t.Fatalf("rotation should end with OK and no file end message: %v", stream.Err())
	}

	w.fake.SetFile(w.osOrg, sandboxID, "/home/dev/work/"+path, []byte("replacement"))
	next := chunk.GetOffset() + int64(len(chunk.GetData()))
	stream = read(next)
	if !stream.Receive() {
		t.Fatalf("missing resumed chunk: %v", stream.Err())
	}
	chunk = stream.Msg().GetChunk()
	if chunk.GetOffset() != next || !bytes.Equal(chunk.GetData(), data[next:]) {
		t.Fatal("resumed chunk did not match the rest of the original snapshot")
	}
	if !stream.Receive() || stream.Msg().GetEnd().GetSizeBytes() != int64(len(data)) || stream.Receive() || stream.Err() != nil {
		t.Fatalf("resumed read should complete with the original size and OK: %v", stream.Err())
	}
}
