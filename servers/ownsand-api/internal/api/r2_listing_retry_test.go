// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type r2ListingPage struct {
	token string
	body  string
}

// r2RetryControl runs worker passes against the signed R2 client. The HTTP
// fixture bounds malformed sequences and serves a complete listing on retry.
func r2RetryControl(t *testing.T, env *testenv.Env, prefix string, pages []r2ListingPage) (*api.Control, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=fixture/") {
			t.Error("missing R2 request signature")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query()
			token := q.Get("continuation-token")
			pageIndex := len(requests)
			requests = append(requests, "list "+token)
			if r.URL.Path != "/bucket" || q.Get("list-type") != "2" || q.Get("prefix") != prefix {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if pageIndex < len(pages) && token == pages[pageIndex].token {
				_, _ = fmt.Fprint(w, pages[pageIndex].body)
				return
			}
			if pageIndex != len(pages) || token != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Contents><Key>%s1</Key><Size>1048576</Size></Contents><Contents><Key>%s2</Key><Size>1048576</Size></Contents></ListBucketResult>`, prefix, prefix)
		case http.MethodDelete:
			requests = append(requests, "delete "+r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	store := &objectstore.R2{Endpoint: server.URL, Bucket: "bucket", AccessKeyID: "fixture", SecretAccessKey: "test", Client: server.Client()}
	control := api.NewControl(&api.Service{
		DB: env.DB, Logger: env.Logger, Now: env.Clock.Now, Objects: store,
		Timing: api.Timing{ExportVerifyPoll: verifyPoll},
	})
	return control, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

func TestCleanupAndExportVerificationRetryInvalidR2Listings(t *testing.T) {
	const truncatedPage = `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken></ListBucketResult>`
	for _, tc := range []struct {
		name  string
		pages []r2ListingPage
	}{
		{
			name: "wrong XML root",
			pages: []r2ListingPage{
				{"", `<Error><Code>InternalError</Code></Error>`},
			},
		},
		{
			name: "repeated continuation token",
			pages: []r2ListingPage{
				{"", truncatedPage},
				{"next", truncatedPage},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failureRequests []string
			for _, page := range tc.pages {
				failureRequests = append(failureRequests, "list "+page.token)
			}
			t.Run("cleanup", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				env := testenv.New(t)
				org := env.NewOrg("alice")
				if _, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
					t.Fatal(err)
				}
				prefix := "organizations/" + org.ID + "/"
				// Exercise the final sweep, which retires the job on success.
				if _, err := env.DB.Pool.Exec(ctx, "UPDATE r2_cleanup_jobs SET first_sweep_done = true WHERE prefix = $1", prefix); err != nil {
					t.Fatal(err)
				}
				env.Clock.Advance(20 * time.Minute)
				control, requests := r2RetryControl(t, env, prefix, tc.pages)
				control.RunCleanupOnce(ctx)

				var attempts int
				var firstDone bool
				var nextAttempt time.Time
				if err := env.DB.Pool.QueryRow(ctx, "SELECT attempts, first_sweep_done, next_attempt_time FROM r2_cleanup_jobs WHERE prefix = $1", prefix).
					Scan(&attempts, &firstDone, &nextAttempt); err != nil {
					t.Fatal(err)
				}
				if attempts != 1 || !firstDone || !nextAttempt.Equal(env.Clock.Now().Add(2*time.Second)) {
					t.Fatalf("cleanup retry state = (%d, %v, %v)", attempts, firstDone, nextAttempt)
				}
				if got := requests(); !slices.Equal(got, failureRequests) {
					t.Fatalf("failed cleanup requests = %q, want %q and zero deletes", got, failureRequests)
				}

				env.Clock.Advance(2 * time.Second)
				control.RunCleanupOnce(ctx)
				if jobs := cleanupJobs(t, env); len(jobs) != 0 {
					t.Fatalf("retired cleanup job remains: %v", jobs)
				}
				want := append(slices.Clone(failureRequests), "list ", "delete /bucket/"+prefix+"1", "delete /bucket/"+prefix+"2")
				if got := requests(); !slices.Equal(got, want) {
					t.Fatalf("cleanup requests after retry = %q, want %q", got, want)
				}
			})
			t.Run("export verification", func(t *testing.T) {
				f := newExportVerifyFixture(t)
				control, requests := r2RetryControl(t, f.env, f.prefix, tc.pages)
				f.env.Clock.Advance(verifyDelay)
				control.RunExportVerifyOnce(context.Background())
				f.wantExported(t, false)
				_, _, after := f.verification(t)
				if after == nil || !after.Equal(f.env.Clock.Now().Add(verifyPoll)) {
					t.Fatalf("export verification retry time = %v", after)
				}
				if got := len(manifestHashes(t, f.env, f.snapID)); got != 2 {
					t.Fatalf("manifest has %d parts after listing failure, want 2", got)
				}
				if jobs := cleanupJobs(t, f.env); len(jobs) != 0 {
					t.Fatalf("verification scheduled cleanup after listing failure: %v", jobs)
				}
				if got := requests(); !slices.Equal(got, failureRequests) {
					t.Fatalf("failed export verification requests = %q, want %q", got, failureRequests)
				}

				f.env.Clock.Advance(verifyPoll)
				control.RunExportVerifyOnce(context.Background())
				f.wantExported(t, true)
				if got, want := requests(), append(slices.Clone(failureRequests), "list "); !slices.Equal(got, want) {
					t.Fatalf("export verification requests after retry = %q, want %q", got, want)
				}
			})
		})
	}
}
