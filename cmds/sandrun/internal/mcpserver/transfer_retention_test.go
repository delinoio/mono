// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/sandrun"
)

func TestTransferStatusRetainsMostRecentCompletions(t *testing.T) {
	client, err := sandrun.New(sandrun.Options{BaseURL: "http://127.0.0.1:1", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Client: func(context.Context) (*sandrun.Client, error) { return client, nil }})
	defer s.Close()

	const limit = 64
	progressReady := make(chan struct{})
	release := make(chan struct{})
	active, err := s.startTransfer("sync", func(ctx context.Context, t *transfer) {
		t.onProgress(sandrun.TransferProgress{Phase: sandrun.TransferUploading, BytesDone: 1})
		close(progressReady)
		select {
		case <-release:
			t.sync = &sandrun.SyncResult{Uploaded: 1}
		case <-ctx.Done():
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-progressReady

	noWait := 0
	status := func(id string) (transferResult, error) {
		_, got, err := s.transferStatus(t.Context(), nil, transferStatusArgs{TransferID: id, WaitSeconds: &noWait})
		return got, err
	}
	checkSize := func(want int) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		if got := len(s.transfers); got != want {
			t.Fatalf("transfer registry has %d entries, want %d", got, want)
		}
	}
	checkEvicted := func(id string) {
		t.Helper()
		if _, err := status(id); err == nil || !strings.HasPrefix(err.Error(), "not_found:") ||
			!strings.Contains(err.Error(), "64 most recent") {
			t.Fatalf("evicted transfer %s: %v", id, err)
		}
	}

	type outcome struct {
		result transferResult
		err    string
	}
	outcomes := make([]outcome, 0, limit+1)
	failure := connect.NewError(connect.CodeFailedPrecondition, context.Canceled)
	for i := range limit + 1 {
		// The first failure can be evicted and the second can still be polled.
		var want outcome
		switch {
		case i < 2:
			want = outcome{result: transferResult{Kind: "download"}, err: "failed_precondition: context canceled"}
		case i%2 == 0:
			want = outcome{result: transferResult{Kind: "sync", Sync: &sandrun.SyncResult{Uploaded: i}}}
		default:
			want = outcome{result: transferResult{Kind: "download", Download: &sandrun.DownloadResult{Files: i}}}
		}
		expected := want.result
		failed := want.err != ""
		entry, err := s.startTransfer(expected.Kind, func(_ context.Context, t *transfer) {
			t.sync, t.download = expected.Sync, expected.Download
			if failed {
				t.err = failure
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entry.done
		checkSize(min(i+1, limit) + 1)
		want.result.TransferID = entry.id
		outcomes = append(outcomes, want)
	}

	checkEvicted(outcomes[0].result.TransferID)
	for _, want := range outcomes[1:] {
		for range 2 {
			got, err := status(want.result.TransferID)
			if want.err != "" {
				if err == nil || want.err != err.Error() {
					t.Fatalf("retained failure %s: %v, want %q", want.result.TransferID, err, want.err)
				}
			} else if err != nil || !reflect.DeepEqual(got, want.result) {
				t.Fatalf("retained result %+v, %v, want %+v", got, err, want.result)
			}
		}
	}
	for range 2 {
		got, err := status(active.id)
		if err != nil || !got.Running || got.Progress == nil || got.Progress.Phase != "uploading" || got.Progress.BytesDone != 1 {
			t.Fatalf("active transfer: %+v, %v", got, err)
		}
	}

	close(release)
	<-active.done
	checkSize(limit)
	checkEvicted(outcomes[1].result.TransferID)
	if got, err := status(active.id); err != nil || got.Running || got.Sync == nil || got.Sync.Uploaded != 1 {
		t.Fatalf("most recent completion: %+v, %v", got, err)
	}
}

func TestTransferStatusInFlightSurvivesEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{})
		defer s.Close()

		release := make(chan struct{})
		want := &sandrun.DownloadResult{Files: 1, Bytes: 7}
		first, err := s.startTransfer("download", func(ctx context.Context, t *transfer) {
			t.download = want
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			result transferResult
			err    error
		}
		waited := make(chan outcome, 1)
		go func() {
			_, result, err := s.transferStatus(t.Context(), nil, transferStatusArgs{TransferID: first.id})
			waited <- outcome{result, err}
		}()
		synctest.Wait()

		func() {
			// The status call has acquired first. Hold its result lock so it
			// cannot read the outcome until after the registry evicts it.
			first.mu.Lock()
			defer first.mu.Unlock()
			close(release)
			<-first.done
			for range 64 {
				newer, err := s.startTransfer("sync", func(context.Context, *transfer) {})
				if err != nil {
					t.Fatal(err)
				}
				<-newer.done
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.transfers[first.id] != nil {
				t.Fatal("first transfer still registered")
			}
		}()

		got := <-waited
		if got.err != nil || got.result.Running || got.result.TransferID != first.id ||
			!reflect.DeepEqual(got.result.Download, want) {
			t.Fatalf("in-flight status after eviction: %+v, %v", got.result, got.err)
		}
	})
}
