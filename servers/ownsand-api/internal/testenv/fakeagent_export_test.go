// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
)

func TestFakeAgentExportResults(t *testing.T) {
	t.Parallel()

	const commandID = "export-command"
	const invalidURL = "memory://PUT/%zz?signature=fixture"
	const firstURL = "memory://PUT/snapshots/part-1?expires=1000"
	const secondURL = "memory://PUT/snapshots/part-2?expires=1000"

	grantError := &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, Message: "upload denied"}
	for _, tc := range []struct {
		name       string
		grant      *ownsandv1.SignedUrlGrant
		skipUpload bool
		wantError  *ownsandv1.ErrorDetail
		wantURLs   []string
	}{
		{
			name:  "grant with another request ID",
			grant: &ownsandv1.SignedUrlGrant{RequestId: "another-command", Error: grantError},
			wantError: &ownsandv1.ErrorDetail{
				Code: ownsandv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, Message: "timed out waiting for signed URL grant",
			},
		},
		{
			name: "invalid upload URL",
			grant: &ownsandv1.SignedUrlGrant{RequestId: commandID, PartUrls: []*ownsandv1.SignedPartUrl{
				{PartNumber: 1, Url: invalidURL},
				{PartNumber: 2, Url: secondURL},
			}},
			wantError: &ownsandv1.ErrorDetail{
				Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, Message: "failed to parse snapshot part upload URL",
			},
		},
		{
			name:      "server grant error",
			grant:     &ownsandv1.SignedUrlGrant{RequestId: commandID, Error: grantError},
			wantError: grantError,
		},
		{
			name: "successful export",
			grant: &ownsandv1.SignedUrlGrant{RequestId: commandID, PartUrls: []*ownsandv1.SignedPartUrl{
				{PartNumber: 1, Url: firstURL},
				{PartNumber: 2, Url: secondURL},
			}},
			wantURLs: []string{firstURL, secondURL},
		},
		{
			name: "store without PutSized skips upload",
			grant: &ownsandv1.SignedUrlGrant{RequestId: commandID, PartUrls: []*ownsandv1.SignedPartUrl{
				{PartNumber: 1, Url: invalidURL},
				{PartNumber: 2, Url: secondURL},
			}},
			skipUpload: true,
			wantURLs:   []string{invalidURL, secondURL},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			memory := objectstore.NewMemory(nil)
			var store objectstore.Store = memory
			if tc.skipUpload {
				store = struct{ objectstore.Store }{memory}
			}

			start := time.Now()
			got := runFakeAgentExport(t, store, commandID, tc.grant)
			if tc.grant.GetRequestId() != commandID && time.Since(start) < 10*time.Second {
				t.Fatal("export returned before the grant deadline")
			}

			want := &ownsandv1.AgentCommandResult{CommandId: commandID, Error: tc.wantError}
			if tc.wantError == nil {
				manifest := &ownsandv1.ExportSnapshotResult{SizeBytes: 2 << 20}
				for i, url := range tc.wantURLs {
					sum := sha256.Sum256([]byte(url))
					manifest.Parts = append(manifest.Parts, &ownsandv1.SnapshotPart{
						PartNumber: int32(i + 1), SizeBytes: 1 << 20, Sha256: sum[:],
					})
				}
				want.Result = &ownsandv1.AgentCommandResult_ExportSnapshot{ExportSnapshot: manifest}
			}
			if !proto.Equal(got, want) {
				t.Fatalf("export result = %v, want %v", got, want)
			}

			var wantObjects []objectstore.Object
			if !tc.skipUpload && tc.wantError == nil {
				wantObjects = []objectstore.Object{
					{Key: "snapshots/part-1", Size: 1 << 20},
					{Key: "snapshots/part-2", Size: 1 << 20},
				}
			}
			if objects, err := memory.ListPrefix(t.Context(), ""); err != nil || !slices.Equal(objects, wantObjects) {
				t.Fatalf("uploaded objects = %v, %v, want %v", objects, err, wantObjects)
			}
		})
	}
}

func runFakeAgentExport(t *testing.T, store objectstore.Store, commandID string, grant *ownsandv1.SignedUrlGrant) *ownsandv1.AgentCommandResult {
	t.Helper()
	result := make(chan *ownsandv1.AgentCommandResult, 1)
	handler := connect.NewBidiStreamHandler(ownsandv1connect.AgentServiceChannelProcedure,
		func(_ context.Context, stream *connect.BidiStream[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) error {
			const snapshotID = "test-snapshot"
			if _, err := stream.Receive(); err != nil {
				return err
			}
			cmd := &ownsandv1.AgentCommand{CommandId: commandID, Command: &ownsandv1.AgentCommand_ExportSnapshot{
				ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: snapshotID},
			}}
			if err := stream.Send(&ownsandv1.ChannelResponse{Envelope: &ownsandv1.AgentEnvelope{
				Message: &ownsandv1.AgentEnvelope_Command{Command: cmd},
			}}); err != nil {
				return err
			}
			req, err := stream.Receive()
			if err != nil {
				return err
			}
			wantReq := &ownsandv1.SignedUrlRequest{
				RequestId: commandID, SnapshotId: snapshotID,
				Operation: ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD, PartNumbers: []int32{1, 2},
			}
			if !proto.Equal(req.GetEnvelope().GetSignedUrlRequest(), wantReq) {
				return errors.New("unexpected signed URL request")
			}
			if err := stream.Send(&ownsandv1.ChannelResponse{Envelope: &ownsandv1.AgentEnvelope{
				Message: &ownsandv1.AgentEnvelope_SignedUrlGrant{SignedUrlGrant: grant},
			}}); err != nil {
				return err
			}
			reply, err := stream.Receive()
			if err != nil {
				return err
			}
			result <- reply.GetEnvelope().GetCommandResult()
			if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
				return fmt.Errorf("expected the request stream to end after one result, got %v", err)
			}
			return nil
		})
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	a := &FakeAgent{
		T: t, Env: &Env{opts: app.Options{Objects: store}},
		ctx: ctx, cancel: cancel, closed: make(chan struct{}), changed: make(chan struct{}),
		stream: ownsandv1connect.NewAgentServiceClient(server.Client(), server.URL).Channel(ctx),
	}
	go a.read()
	t.Cleanup(a.Close)
	a.Heartbeat(1)

	var got *ownsandv1.AgentCommandResult
	select {
	case got = <-result:
	case <-a.Closed():
		t.Fatalf("channel closed before the export result: %v", a.CloseErr())
	case <-ctx.Done():
		t.Fatalf("waiting for the export result: %v", ctx.Err())
	}

	a.sendMu.Lock()
	err := a.stream.CloseRequest()
	a.sendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Closed():
		if err := a.CloseErr(); !errors.Is(err, io.EOF) {
			t.Fatalf("ending the channel after the export result: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for the channel to close: %v", ctx.Err())
	}
	return got
}
