// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func TestReadFileRelayDeliveryCancel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		secondChunk bool
		cancel      bool
		wantOffset  int64
		wantData    string
		wantWrites  int
		wantErr     error
	}{
		{name: "cancel_chunk", secondChunk: true, cancel: true, wantOffset: 4, wantData: "cd", wantWrites: 1, wantErr: context.Canceled},
		{name: "cancel_end", cancel: true, wantOffset: 4, wantData: "cd", wantWrites: 1, wantErr: context.Canceled},
		{name: "live_chunk", secondChunk: true, wantOffset: 6, wantData: "cdef", wantWrites: 2},
		{name: "live_end", wantOffset: 4, wantData: "cd", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			wire, cancelAfter := relayReadFixture(t, tc.secondChunk)
			body := &relayReadCancelBody{Reader: bytes.NewReader(wire), remainingAtCancel: len(wire) - cancelAfter}
			if tc.cancel {
				body.cancel = cancel
			}
			grant, err := proto.Marshal(&ownsandv1.AuthorizeRelayResponse{
				GrantId:   "test-grant",
				ExpiresIn: durationpb.New(15 * time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			var grants, reads int
			transport := relayReadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if err := req.Body.Close(); err != nil {
					return nil, err
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
				switch req.URL.Path {
				case ownsandv1connect.DataPathServiceAuthorizeRelayProcedure:
					grants++
					resp.Header.Set("Content-Type", "application/proto")
					resp.Body = io.NopCloser(bytes.NewReader(grant))
				case ownsandv1connect.RelayServiceReadFileProcedure:
					reads++
					if reads > 1 {
						return nil, fmt.Errorf("unexpected relay continuation")
					}
					resp.Header.Set("Content-Type", "application/connect+proto")
					resp.Body = body
				default:
					return nil, fmt.Errorf("unexpected request path: %s", req.URL.Path)
				}
				return resp, nil
			})
			client, err := New(Options{
				BaseURL:        "http://ownsand.test",
				APIKey:         "test-key",
				OrganizationID: "0192f4b6-7c3a-7def-8abc-0123456789ab",
				HTTPClient:     &http.Client{Transport: transport},
				Transport:      TransportRelay,
			})
			if err != nil {
				t.Fatal(err)
			}
			sandbox, err := client.Sandbox("", "0192f4b6-7c3a-7def-8abc-0123456789ac")
			if err != nil {
				t.Fatal(err)
			}
			var writer prefixWriter
			offset, err := sandbox.ReadFileFrom(ctx, FileReadParams{
				ReadID: "0192f4b6-7c3a-7def-8abc-0123456789ad",
				Path:   "/file",
				Offset: 2,
			}, &writer)
			if body.canceled != tc.cancel || !body.closed {
				t.Errorf("response body canceled = %t, closed = %t, want %t, true", body.canceled, body.closed, tc.cancel)
			}
			if grants != 1 || reads != 1 {
				t.Errorf("got %d grant requests and %d relay reads, want 1 each", grants, reads)
			}
			if offset != tc.wantOffset || err != tc.wantErr {
				t.Errorf("ReadFileFrom = (%d, %v), want (%d, %v)", offset, err, tc.wantOffset, tc.wantErr)
			}
			if got := writer.buf.String(); got != tc.wantData || writer.calls != tc.wantWrites {
				t.Errorf("delivered %q in %d writes, want %q in %d writes", got, writer.calls, tc.wantData, tc.wantWrites)
			}
		})
	}
}

func relayReadFixture(t *testing.T, secondChunk bool) (wire []byte, cancelAfter int) {
	t.Helper()
	events := []*ownsandv1.FileReadEvent{
		{Event: &ownsandv1.FileReadEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}},
		{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 2, Data: []byte("cd")}}},
	}
	size := int64(4)
	if secondChunk {
		events = append(events, &ownsandv1.FileReadEvent{
			Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 4, Data: []byte("ef")}},
		})
		size = 6
	}
	events = append(events, &ownsandv1.FileReadEvent{
		Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: size}},
	})
	for i, event := range events {
		payload, err := proto.Marshal(&ownsandv1.ReadFileResponse{Event: event})
		if err != nil {
			t.Fatal(err)
		}
		// Each Connect envelope starts with a flag byte and a four-byte payload length.
		wire = binary.BigEndian.AppendUint32(append(wire, 0), uint32(len(payload)))
		wire = append(wire, payload...)
		if i == 2 {
			cancelAfter = len(wire)
		}
	}
	// End-of-stream envelope with empty Connect metadata and no error.
	wire = append(wire, 0x02, 0, 0, 0, 2, '{', '}')
	return wire, cancelAfter
}

type relayReadRoundTripFunc func(*http.Request) (*http.Response, error)

func (f relayReadRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type relayReadCancelBody struct {
	*bytes.Reader
	remainingAtCancel int
	cancel            context.CancelFunc
	canceled          bool
	closed            bool
}

func (b *relayReadCancelBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	// Cancel after filling the last payload read for the selected event.
	// Connect still receives these bytes successfully.
	if b.cancel != nil && !b.canceled && n > 0 && err == nil && b.Len() == b.remainingAtCancel {
		b.canceled = true
		b.cancel()
	}
	return n, err
}

func (b *relayReadCancelBody) Close() error {
	b.closed = true
	return nil
}
