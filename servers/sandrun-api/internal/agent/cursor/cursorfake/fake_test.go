// SPDX-License-Identifier: FSL-1.1-ALv2

package cursorfake_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/cursor/cursorfake"
)

func TestReleasedClaimRequeuesOnlyUnstartedDemand(t *testing.T) {
	t.Parallel()

	t.Run("release_and_reclaim", func(t *testing.T) {
		f := cursorfake.New(t, "test-key", "test-pool")
		original := f.AddRequest("bc-release", false)
		claimRequest(t, f, original.ID, "worker-1")
		expectPending(t, f)

		// A release that stamps a new time must differ from the original demand.
		time.Sleep(time.Until(time.UnixMilli(original.CreatedAtMs + 1)))
		path := "/claims/" + original.ID + "/release"
		callFake(t, f, http.MethodPost, path, "", http.StatusOK, nil)
		if worker, held := f.Claim(original.ID); held {
			t.Fatalf("claim still held by %q after release", worker)
		}
		callFake(t, f, http.MethodPost, path, "", http.StatusNotFound, nil)
		expectPending(t, f, original)

		claimRequest(t, f, original.ID, "worker-2")
		if worker, held := f.Claim(original.ID); !held || worker != "worker-2" {
			t.Fatalf("claim after reclaim = (%q, %t), want worker-2", worker, held)
		}
		expectPending(t, f)
	})

	t.Run("fail_ends_waiting_turn", func(t *testing.T) {
		f := cursorfake.New(t, "test-key", "test-pool")
		original := f.AddRequest("bc-fail", false)
		claimRequest(t, f, original.ID, "worker-1")

		callFake(t, f, http.MethodPost, "/claims/"+original.ID+"/fail", `{"message":"runner unavailable"}`, http.StatusOK, nil)
		if worker, held := f.Claim(original.ID); held {
			t.Fatalf("claim still held by %q after fail", worker)
		}
		expectPending(t, f)
	})

	t.Run("release_after_connect", func(t *testing.T) {
		f := cursorfake.New(t, "test-key", "test-pool")
		original := f.AddRequest("bc-connected", false)
		claimRequest(t, f, original.ID, "worker-1")
		f.Connect("worker-1")

		callFake(t, f, http.MethodPost, "/claims/"+original.ID+"/release", "", http.StatusOK, nil)
		if worker, held := f.Claim(original.ID); held {
			t.Fatalf("claim still held by %q after release", worker)
		}
		expectPending(t, f)
	})

	t.Run("release_after_transient_failure", func(t *testing.T) {
		f := cursorfake.New(t, "test-key", "test-pool")
		original := f.AddRequest("bc-retry-release", false)
		claimRequest(t, f, original.ID, "worker-1")

		path := "/claims/" + original.ID + "/release"
		f.FailClaimCalls(true)
		callFake(t, f, http.MethodPost, path, "", http.StatusServiceUnavailable, nil)
		if worker, held := f.Claim(original.ID); !held || worker != "worker-1" {
			t.Fatalf("claim after failed release = (%q, %t), want worker-1", worker, held)
		}
		expectPending(t, f)

		f.FailClaimCalls(false)
		callFake(t, f, http.MethodPost, path, "", http.StatusOK, nil)
		expectPending(t, f, original)
	})
}

func claimRequest(t *testing.T, f *cursorfake.Fake, id, workerID string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"workerId":%q}`, id, workerID)
	callFake(t, f, http.MethodPost, "/claim", body, http.StatusOK, nil)
}

func expectPending(t *testing.T, f *cursorfake.Fake, want ...cursorfake.Request) {
	t.Helper()
	var page cursorfake.Page
	callFake(t, f, http.MethodGet, "/pending-requests?pool="+f.Pool, "", http.StatusOK, &page)
	if !slices.Equal(page.Requests, want) {
		t.Fatalf("pending requests = %+v, want %+v", page.Requests, want)
	}
	t.Logf("pending requests = %+v", page.Requests)
}

func callFake(t *testing.T, f *cursorfake.Fake, method, path, body string, wantStatus int, result any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.Server.URL+"/v0/private-workers"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(f.Key, "")
	resp, err := f.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, resp.StatusCode, wantStatus, body)
	}
	t.Logf("%s %s returned %d", method, path, resp.StatusCode)
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
}
