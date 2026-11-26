// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestWatchQueueContextAtDispatch(t *testing.T) {
	const requestID = "request-1"
	payload, err := proto.Marshal(&sandrunv1.WatchQueueResponse{
		Event: &sandrunv1.WatchQueueResponse_Request{Request: &sandrunv1.Request{RequestId: requestID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var prefix [5]byte
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(payload)))

	for _, tc := range []struct {
		name           string
		wantContextErr error
	}{
		{name: "canceled", wantContextErr: context.Canceled},
		{name: "deadline", wantContextErr: context.DeadlineExceeded},
		{name: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.wantContextErr == context.DeadlineExceeded {
					deadlineCtx, cancelDeadline := context.WithTimeout(ctx, time.Second)
					defer cancelDeadline()
					ctx = deadlineCtx
				}

				body := &watchTestBody{
					prefix:  bytes.NewReader(prefix[:]),
					payload: bytes.NewReader(payload),
					beforePayload: func() {
						switch tc.wantContextErr {
						case context.Canceled:
							cancel()
						case context.DeadlineExceeded:
							<-ctx.Done()
						}
					},
				}
				requests := 0
				client, err := New(Options{
					BaseURL: "http://127.0.0.1:18400",
					APIKey:  "test-token",
					HTTPClient: &http.Client{Transport: watchTestTransport(func(req *http.Request) (*http.Response, error) {
						requests++
						_ = req.Body.Close()
						if req.URL.Path != sandrunv1connect.WorkServiceWatchQueueProcedure {
							t.Errorf("RPC path = %q, want WatchQueue", req.URL.Path)
						}
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{"Content-Type": {"application/connect+proto"}},
							Body:       body,
						}, nil
					})},
				})
				if err != nil {
					t.Fatal(err)
				}

				handlerErr := errors.New("stop after request")
				var events []QueueEvent
				err = client.WatchQueue(ctx, "test-org", func(event QueueEvent) error {
					events = append(events, event)
					if ctx.Err() == nil {
						// An admitted callback keeps its error even if it cancels the watch.
						cancel()
					}
					return handlerErr
				})
				if tc.wantContextErr != nil {
					if err != tc.wantContextErr || len(events) != 0 {
						t.Errorf("WatchQueue returned %v with %d events, want %v and no events", err, len(events), tc.wantContextErr)
					}
				} else if err != handlerErr || len(events) != 1 || events[0].Kind != QueueEventRequest || events[0].Request.GetRequestId() != requestID {
					t.Errorf("WatchQueue returned %v with events %v, want the original handler error and the request event", err, events)
				}
				if requests != 1 || !body.closed || body.payload.Len() != 0 {
					t.Errorf("RPCs = %d, body closed = %t, unread payload = %d, want 1, true, 0", requests, body.closed, body.payload.Len())
				}
			})
		})
	}
}

type watchTestTransport func(*http.Request) (*http.Response, error)

func (transport watchTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

type watchTestBody struct {
	prefix        *bytes.Reader
	payload       *bytes.Reader
	beforePayload func()
	closed        bool
}

func (body *watchTestBody) Read(p []byte) (int, error) {
	if body.prefix.Len() > 0 {
		return body.prefix.Read(p)
	}
	if body.beforePayload != nil {
		// Connect has checked the context and is about to receive the complete payload.
		body.beforePayload()
		body.beforePayload = nil
	}
	return body.payload.Read(p)
}

func (body *watchTestBody) Close() error {
	body.closed = true
	return nil
}
