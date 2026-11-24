// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

const testWriteKey = "sandbox/write"

func acquireTestWrite(t *testing.T, w *writes) *writeEntry {
	t.Helper()
	e, err := w.acquire(testWriteKey, "/file", func() (ownsandclient.FileWrite, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func checkCachedWrite(t *testing.T, w *writes, want *writeEntry) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if got := w.entries[testWriteKey]; got != want {
		t.Fatalf("cached write = %p, want %p", got, want)
	}
}

func TestWriteExpiresWithoutAnotherAcquire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWrites()
		e := acquireTestWrite(t, w)
		w.release(testWriteKey, e, false, nil)

		time.Sleep(19 * time.Minute)
		checkCachedWrite(t, w, e)
		time.Sleep(time.Minute)
		synctest.Wait()
		checkCachedWrite(t, w, nil)
	})
}

func TestWriteRefreshSurvivesExpiryDuringChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWrites()
		e := acquireTestWrite(t, w)
		w.release(testWriteKey, e, false, nil)

		time.Sleep(19 * time.Minute)
		continuation := acquireTestWrite(t, w)
		if continuation != e {
			t.Errorf("continuation = %p, want original entry %p", continuation, e)
		}
		time.Sleep(time.Minute)
		checkCachedWrite(t, w, e)
		w.release(testWriteKey, continuation, false, nil)
		synctest.Wait()
		checkCachedWrite(t, w, e)

		time.Sleep(writeIdle - time.Nanosecond)
		checkCachedWrite(t, w, e)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		checkCachedWrite(t, w, nil)
	})
}

func TestWriteFailureDoesNotRefreshExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWrites()
		e := acquireTestWrite(t, w)
		w.release(testWriteKey, e, false, nil)

		time.Sleep(19 * time.Minute)
		continuation := acquireTestWrite(t, w)
		w.release(testWriteKey, continuation, false, connect.NewError(connect.CodeUnavailable, errors.New("upload unavailable")))
		time.Sleep(time.Minute)
		synctest.Wait()
		checkCachedWrite(t, w, nil)
	})
}

func TestWriteTerminalReleasePreservesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		finished bool
		err      error
	}{
		{name: "finalized", finished: true},
		{name: "guest not found", err: connect.NewError(connect.CodeNotFound, errors.New("write expired"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWrites()
				original := acquireTestWrite(t, w)
				w.release(testWriteKey, original, false, nil)

				time.Sleep(19 * time.Minute)
				terminal := acquireTestWrite(t, w)
				w.release(testWriteKey, terminal, tc.finished, tc.err)
				checkCachedWrite(t, w, nil)

				replacement := acquireTestWrite(t, w)
				w.release(testWriteKey, replacement, false, nil)
				if replacement == original {
					t.Fatal("replacement reused the removed entry")
				}
				time.Sleep(time.Minute)
				// Run a stale callback as if it had already started when the timer stopped.
				w.expire(testWriteKey, original)
				checkCachedWrite(t, w, replacement)
			})
		})
	}
}
