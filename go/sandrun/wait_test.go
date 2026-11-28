// SPDX-License-Identifier: Apache-2.0

package sandrun_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/sandrun"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type waitTransport func(*http.Request) (*http.Response, error)

func (f waitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type waitBodyReader func([]byte) (int, error)

func (f waitBodyReader) Read(p []byte) (int, error) {
	return f(p)
}

func TestWaitDevSessionPollResult(t *testing.T) {
	const (
		empty    = sandrunv1.RequestState_REQUEST_STATE_UNSPECIFIED
		queued   = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		running  = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		expired  = sandrunv1.RequestState_REQUEST_STATE_EXPIRED
		interval = time.Millisecond
	)

	for _, tt := range []struct {
		name          string
		states        []sandrunv1.RequestState
		deadline      time.Duration
		endDuringRead bool
		wantState     sandrunv1.RequestState
		wantErr       error
	}{
		{
			name: "deadline before running", states: []sandrunv1.RequestState{queued, running},
			deadline: time.Second, endDuringRead: true, wantState: queued, wantErr: context.DeadlineExceeded,
		},
		{
			name: "cancel before terminal", states: []sandrunv1.RequestState{queued, expired},
			endDuringRead: true, wantState: queued, wantErr: context.Canceled,
		},
		{
			name: "cancel during first poll", states: []sandrunv1.RequestState{running},
			endDuringRead: true, wantState: empty, wantErr: context.Canceled,
		},
		{
			name: "deadline after empty poll", states: []sandrunv1.RequestState{queued, empty},
			deadline: 1500 * time.Microsecond, wantState: queued, wantErr: context.DeadlineExceeded,
		},
		{
			name: "deadline with no accepted session", states: []sandrunv1.RequestState{empty},
			deadline: 500 * time.Microsecond, wantState: empty, wantErr: context.DeadlineExceeded,
		},
		{
			name: "running after empty poll", states: []sandrunv1.RequestState{queued, empty, running}, wantState: running,
		},
		{
			name: "terminal after empty poll", states: []sandrunv1.RequestState{queued, empty, expired}, wantState: expired,
		},
		{
			name: "running", states: []sandrunv1.RequestState{queued, running}, wantState: running,
		},
		{
			name: "terminal", states: []sandrunv1.RequestState{expired}, wantState: expired,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				cancel := func() {}
				switch tt.wantErr {
				case context.Canceled:
					ctx, cancel = context.WithCancel(ctx)
				case context.DeadlineExceeded:
					ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				}
				defer cancel()

				polls := 0
				start := time.Now()
				transport := waitTransport(func(req *http.Request) (*http.Response, error) {
					defer req.Body.Close()
					if req.URL.Path != sandrunv1connect.DevSessionServiceGetDevSessionProcedure {
						t.Fatalf("unexpected request path %q", req.URL.Path)
					}
					polls++
					if polls > len(tt.states) {
						t.Fatalf("unexpected poll %d", polls)
					}
					if elapsed, want := time.Since(start), time.Duration(polls-1)*interval; elapsed != want {
						t.Errorf("poll %d started after %v, want %v", polls, elapsed, want)
					}
					reply := &sandrunv1.GetDevSessionResponse{}
					if state := tt.states[polls-1]; state != empty {
						reply.DevSession = &sandrunv1.DevSession{DevSessionId: "session", State: state}
					}
					payload, err := proto.Marshal(reply)
					if err != nil {
						t.Fatal(err)
					}
					var body io.Reader = bytes.NewReader(payload)
					if polls == len(tt.states) && tt.endDuringRead {
						body = waitBodyReader(func(p []byte) (int, error) {
							if err := req.Context().Err(); err != nil {
								t.Fatalf("context ended before body read: %v", err)
							}
							if tt.wantErr == context.Canceled {
								cancel()
							}
							<-req.Context().Done()
							// Return the full message with EOF so Connect can decode
							// it without another read of the canceled request.
							n := copy(p, payload)
							if n != len(payload) {
								t.Fatalf("read buffer has %d bytes, need %d", len(p), len(payload))
							}
							return n, io.EOF
						})
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/proto"}},
						Body:       io.NopCloser(body),
					}, nil
				})

				client, err := sandrun.New(sandrun.Options{
					BaseURL:    "https://example.test",
					APIKey:     "test-key",
					HTTPClient: &http.Client{Transport: transport},
				})
				if err != nil {
					t.Fatal(err)
				}
				session, err := client.WaitDevSession(ctx, sandrun.WaitDevSessionParams{
					OrganizationID: "org",
					DevSessionID:   "session",
					Interval:       interval,
				})
				var want *sandrunv1.DevSession
				if tt.wantState != empty {
					want = &sandrunv1.DevSession{DevSessionId: "session", State: tt.wantState}
				}
				if !proto.Equal(session, want) || err != tt.wantErr || polls != len(tt.states) {
					t.Errorf("wait = (%v, %v) after %d polls, want (%v, %v) after %d polls",
						session, err, polls, want, tt.wantErr, len(tt.states))
				}
			})
		})
	}
}
