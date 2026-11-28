// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"math"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func TestFakeRelayWriteFile(t *testing.T) {
	t.Parallel()

	const path = "/tmp/write"
	start := func(t *testing.T) (func(*ownsandv1.WriteFileRequest, connect.Code) *ownsandv1.WriteFileResponse, func(string), string) {
		t.Helper()
		r := newRunner(t)
		client := ownsandv1connect.NewRelayServiceClient(r.f.srv.Client(), r.f.srv.URL)
		write := func(req *ownsandv1.WriteFileRequest, wantCode connect.Code) *ownsandv1.WriteFileResponse {
			t.Helper()
			req.OrganizationId, req.SandboxId = r.f.org, r.f.sb
			resp, err := client.WriteFile(t.Context(), connect.NewRequest(req))
			if wantCode != 0 {
				if connect.CodeOf(err) != wantCode {
					t.Fatalf("write at offset %d: error %v, want %v", req.GetOffset(), err, wantCode)
				}
				return nil
			}
			if err != nil {
				t.Fatalf("write at offset %d: %v", req.GetOffset(), err)
			}
			return resp.Msg
		}
		read := func(want string) {
			t.Helper()
			res := r.run("file", "read", r.f.sb, path)
			if res.code != ExitOK || res.stdout != want {
				t.Fatalf("read: %+v, want contents %q", res, want)
			}
		}
		return write, read, newID(t)
	}

	t.Run("out of order and repeated finish", func(t *testing.T) {
		write, read, id := start(t)
		tail := &ownsandv1.WriteFileRequest{WriteId: id, Path: path, Offset: 3, Data: []byte("def")}
		wantTail := &ownsandv1.WriteFileResponse{WriteId: id, Offset: 3, Length: 3}
		for range 2 {
			if got := write(tail, 0); !proto.Equal(got, wantTail) {
				t.Fatalf("tail response = %v, want %v", got, wantTail)
			}
		}
		finish := &ownsandv1.WriteFileRequest{
			WriteId: id,
			Data:    []byte("abcd"),
			Finish:  &ownsandv1.WriteFileFinish{SizeBytes: 6, Mode: 0o600},
		}
		wantFinish := &ownsandv1.WriteFileResponse{WriteId: id, Length: 4, Finished: true, SizeBytes: 6}
		if got := write(finish, 0); !proto.Equal(got, wantFinish) {
			t.Fatalf("finish response = %v, want %v", got, wantFinish)
		}
		read("abcdef")

		write(&ownsandv1.WriteFileRequest{
			WriteId: newID(t),
			Path:    path,
			Data:    []byte("new"),
			Finish:  &ownsandv1.WriteFileFinish{SizeBytes: 3, Mode: 0o644},
		}, 0)
		if got := write(finish, 0); !proto.Equal(got, wantFinish) {
			t.Fatalf("repeated finish response = %v, want %v", got, wantFinish)
		}
		read("new")
		changedChunk := proto.CloneOf(finish)
		changedChunk.Data = []byte("abc!")
		write(changedChunk, connect.CodeFailedPrecondition)
		read("new")
		finish.Finish.Mode = 0o644
		write(finish, connect.CodeFailedPrecondition)
		read("new")
	})

	t.Run("conflict leaves staging and destination intact", func(t *testing.T) {
		write, read, id := start(t)
		write(&ownsandv1.WriteFileRequest{
			WriteId: newID(t),
			Path:    path,
			Data:    []byte("old"),
			Finish:  &ownsandv1.WriteFileFinish{SizeBytes: 3, Mode: 0o644},
		}, 0)
		write(&ownsandv1.WriteFileRequest{WriteId: id, Path: path, Offset: 2, Data: []byte("cd")}, 0)
		finish := &ownsandv1.WriteFileRequest{WriteId: id, Finish: &ownsandv1.WriteFileFinish{SizeBytes: 4, Mode: 0o644}}
		write(&ownsandv1.WriteFileRequest{WriteId: id, Data: []byte("xx!"), Finish: finish.Finish}, connect.CodeFailedPrecondition)
		read("old")
		write(finish, connect.CodeFailedPrecondition)
		read("old")
		write(&ownsandv1.WriteFileRequest{WriteId: id, Data: []byte("ab")}, 0)
		write(finish, 0)
		read("abcd")
	})

	for _, tc := range []struct {
		name   string
		offset int64
		size   int64
	}{
		{name: "gap within declared size", offset: 1, size: 2},
		{name: "bytes beyond declared size", offset: 0, size: 0},
		{name: "matching byte count with wrong offset", offset: 1, size: 1},
		{name: "sparse offset", offset: math.MaxInt64 - 1, size: math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write, read, id := start(t)
			write(&ownsandv1.WriteFileRequest{
				WriteId: newID(t),
				Path:    path,
				Data:    []byte("old"),
				Finish:  &ownsandv1.WriteFileFinish{SizeBytes: 3, Mode: 0o644},
			}, 0)
			write(&ownsandv1.WriteFileRequest{WriteId: id, Path: path, Offset: tc.offset, Data: []byte("x")}, 0)
			write(&ownsandv1.WriteFileRequest{
				WriteId: id,
				Finish:  &ownsandv1.WriteFileFinish{SizeBytes: tc.size, Mode: 0o644},
			}, connect.CodeFailedPrecondition)
			read("old")
		})
	}

	t.Run("empty file", func(t *testing.T) {
		write, read, id := start(t)
		got := write(&ownsandv1.WriteFileRequest{
			WriteId: id,
			Path:    path,
			Finish:  &ownsandv1.WriteFileFinish{Mode: 0o644},
		}, 0)
		if want := (&ownsandv1.WriteFileResponse{WriteId: id, Finished: true}); !proto.Equal(got, want) {
			t.Fatalf("empty finish response = %v, want %v", got, want)
		}
		read("")
	})
}
