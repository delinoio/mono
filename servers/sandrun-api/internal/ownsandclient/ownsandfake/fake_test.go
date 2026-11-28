// SPDX-License-Identifier: FSL-1.1-ALv2

package ownsandfake_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
)

func TestActiveWritePathBinding(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		finish bool
	}{
		{name: "chunk"},
		{name: "finish", finish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			const org, key = "org", "key"
			const original, other = "/fixture/original.txt", "/fixture/other.txt"
			salt := []byte("0123456789abcdef0123456789abcdef")
			fake := ownsandfake.New()
			fake.AddOrg(org, key)
			client, err := fake.New(org, key)
			if err != nil {
				t.Fatal(err)
			}
			sb, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: "sandbox"})
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := sb.GetSandboxId()
			open := func(id, path string) ownsandclient.FileWrite {
				t.Helper()
				w, err := client.OpenFileWrite(sandboxID, ownsand.FileWriteParams{WriteID: id, Path: path, BindingSalt: salt})
				if err != nil {
					t.Fatal(err)
				}
				return w
			}
			checkFile := func(path, want string) {
				t.Helper()
				got, ok := fake.File(org, sandboxID, path)
				if !ok || string(got) != want {
					t.Fatalf("file %s = %q (exists %t), want %q", path, got, ok, want)
				}
			}

			id := api.NewID()
			if _, err := open(id, original).WriteChunk(ctx, 0, []byte("a")); err != nil {
				t.Fatal(err)
			}
			fake.SetFile(org, sandboxID, other, []byte("existing"))

			changed := open(id, other)
			var res *ownsandv1.WriteFileResponse
			if tc.finish {
				res, err = changed.Finish(ctx, 1, []byte("x"), 2, 0o600)
			} else {
				res, err = changed.WriteChunk(ctx, 1, []byte("x"))
			}
			if res != nil {
				t.Fatalf("changed path returned an acknowledgment: %v", res)
			}
			var dpErr *ownsand.DataPathError
			if !errors.As(err, &dpErr) || dpErr.Operation != "write-file" || dpErr.OperationID != id || dpErr.OutcomeUnknown {
				t.Fatalf("changed path error = %v, want a write-file data path error for %s with a known outcome", err, id)
			}
			var wireErr *connect.Error
			if !errors.As(dpErr.Err, &wireErr) || !connect.IsWireError(wireErr) ||
				wireErr.Code() != connect.CodePermissionDenied ||
				wireErr.Message() != "request does not match the recorded operation" {
				t.Fatalf("wrapped error = %v, want the permission_denied wire error for a mismatched operation", dpErr.Err)
			}
			if got, ok := fake.File(org, sandboxID, original); ok {
				t.Fatalf("original path exists with %q before a matching finish", got)
			}
			checkFile(other, "existing")

			res, err = open(id, original).Finish(ctx, 1, []byte("b"), 2, 0o600)
			if err != nil || !res.GetFinished() || res.GetSizeBytes() != 2 {
				t.Fatalf("finish with original path = %v, %v, want a finished 2-byte write", res, err)
			}
			checkFile(original, "ab")

			res, err = open(api.NewID(), other).Finish(ctx, 0, []byte("new"), 3, 0o600)
			if err != nil || !res.GetFinished() || res.GetSizeBytes() != 3 {
				t.Fatalf("finish with new ID = %v, %v, want a finished 3-byte write", res, err)
			}
			checkFile(other, "new")
		})
	}
}

func TestCompletedWriteReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		const org, key, path = "org", "key", "/work/fixture.txt"
		fake := ownsandfake.New()
		fake.AddOrg(org, key)
		client, err := fake.New(org, key)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: "sandbox"})
		if err != nil {
			t.Fatal(err)
		}
		open := func(id string) ownsandclient.FileWrite {
			t.Helper()
			w, err := client.OpenFileWrite(sb.GetSandboxId(), ownsand.FileWriteParams{WriteID: id, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			return w
		}
		checkFile := func(want string) {
			t.Helper()
			got, ok := fake.File(org, sb.GetSandboxId(), path)
			if !ok || string(got) != want {
				t.Fatalf("file = %q (exists %t), want %q", got, ok, want)
			}
		}

		id := api.NewID()
		first := open(id)
		if _, err := first.WriteChunk(ctx, 0, []byte("abc")); err != nil {
			t.Fatal(err)
		}
		want := &ownsandv1.WriteFileResponse{WriteId: id, Offset: 3, Length: 3, Finished: true, SizeBytes: 6}
		finish := func(w ownsandclient.FileWrite) {
			t.Helper()
			got, err := w.Finish(ctx, 3, []byte("def"), 6, 0o600)
			if err != nil || !proto.Equal(got, want) {
				t.Fatalf("finish = %v, %v, want %v", got, err, want)
			}
			got.SizeBytes = 0 // The caller does not own the stored result.
		}
		finish(first)
		checkFile("abcdef")
		finish(first)
		reopened := open(id)
		finish(reopened)

		if _, err := open(api.NewID()).Finish(ctx, 0, []byte("newer"), 5, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name   string
			offset int64
			data   string
			size   int64
			mode   uint32
		}{
			{name: "offset", offset: 2, data: "def", size: 6, mode: 0o600},
			{name: "data", offset: 3, data: "xyz", size: 6, mode: 0o600},
			{name: "size", offset: 3, data: "def", size: 7, mode: 0o600},
			{name: "mode", offset: 3, data: "def", size: 6, mode: 0o644},
		} {
			if _, err := reopened.Finish(ctx, tc.offset, []byte(tc.data), tc.size, tc.mode); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("changed %s: got %v, want failed_precondition", tc.name, err)
			}
		}
		if _, err := reopened.WriteChunk(ctx, 0, []byte("abc")); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("non-final chunk: got %v, want failed_precondition", err)
		}
		time.Sleep(15*time.Minute - time.Millisecond)
		finish(reopened)
		checkFile("newer")

		time.Sleep(time.Millisecond)
		if _, err := reopened.Finish(ctx, 3, []byte("def"), 6, 0o600); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("finish at expiration: got %v, want not_found", err)
		}
		if _, err := open(id).WriteChunk(ctx, 0, []byte("abc")); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("chunk through reopened expired ID: got %v, want not_found", err)
		}
		checkFile("newer")
	})
}
