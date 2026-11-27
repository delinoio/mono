// SPDX-License-Identifier: FSL-1.1-ALv2

package ownsandfake_test

import (
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
