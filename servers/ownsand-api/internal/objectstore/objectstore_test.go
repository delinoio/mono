// SPDX-License-Identifier: FSL-1.1-ALv2

package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPresignAWSVector checks the signer against the presigned URL example
// of the AWS Signature Version 4 documentation ("Authenticating Requests:
// Using Query Parameters").
func TestPresignAWSVector(t *testing.T) {
	r := &R2{
		Endpoint:        "https://examplebucket.s3.amazonaws.com",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
	}
	signed, exp, err := r.presign(http.MethodGet, "test.txt", 24*time.Hour, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	if got := u.Query().Get("X-Amz-Signature"); got != "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404" {
		t.Fatalf("signature = %s", got)
	}
	if !exp.Equal(time.Date(2013, 5, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v", exp)
	}
	if _, _, err := r.presign(http.MethodGet, "x", 8*24*time.Hour, time.Now()); err == nil {
		t.Fatal("a lifetime over 7 days was accepted")
	}
}

func TestPathStyleKey(t *testing.T) {
	r := &R2{Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "snapshots", AccessKeyID: "k", SecretAccessKey: "s", Now: func() time.Time { return time.Unix(0, 0) }}
	signed, _, err := r.PresignPut("organizations/o/snapshots/s/parts/1", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(signed, "https://acct.r2.cloudflarestorage.com/snapshots/organizations/o/snapshots/s/parts/1?") ||
		!strings.Contains(signed, "X-Amz-Expires=900") {
		t.Fatalf("url = %s", signed)
	}
}

func TestDeletePrefix(t *testing.T) {
	const prefix = "organizations/o/"
	var mu sync.Mutex
	objects := map[string]bool{}
	keys := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("organizations/o/snapshots/s/parts/%d", i+1)
		keys = append(keys, key)
		objects[key] = true
	}
	objects["organizations/other/x"] = true
	// The fake serves three fixed pages keyed by the continuation token of the
	// request. An empty token selects the first page; every other token must be
	// exactly the one the previous page returned.
	type page struct {
		key  string
		next string
	}
	pages := map[string]page{
		"":       {key: keys[0], next: "page-2"},
		"page-2": {key: keys[1], next: "page-3"},
		"page-3": {key: keys[2]},
	}
	var tokens []string
	var fixtureErrors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=k/") || req.Header.Get("x-amz-date") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case http.MethodGet:
			query := req.URL.Query()
			token := query.Get("continuation-token")
			tokens = append(tokens, token)
			// Fail fast instead of looping forever when the client never
			// advances through the pages.
			if len(tokens) > len(pages) {
				fixtureErrors = append(fixtureErrors, "too many list requests")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if got := query.Get("prefix"); got != prefix {
				fixtureErrors = append(fixtureErrors, fmt.Sprintf("prefix = %q", got))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			p, ok := pages[token]
			if !ok {
				fixtureErrors = append(fixtureErrors, fmt.Sprintf("unexpected continuation token %q", token))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var b strings.Builder
			b.WriteString("<ListBucketResult>")
			fmt.Fprintf(&b, "<Contents><Key>%s</Key></Contents>", p.key)
			if p.next != "" {
				fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", p.next)
			} else {
				b.WriteString("<IsTruncated>false</IsTruncated>")
			}
			b.WriteString("</ListBucketResult>")
			_, _ = w.Write([]byte(b.String()))
		case http.MethodDelete:
			delete(objects, strings.TrimPrefix(req.URL.Path, "/bucket/"))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	r := &R2{Endpoint: srv.URL, Bucket: "bucket", AccessKeyID: "k", SecretAccessKey: "s"}
	if err := r.DeletePrefix(context.Background(), prefix); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fixtureErrors) != 0 {
		t.Fatalf("fixture errors = %v", fixtureErrors)
	}
	if want := []string{"", "page-2", "page-3"}; !slices.Equal(tokens, want) {
		t.Fatalf("continuation tokens = %q, want %q", tokens, want)
	}
	if len(objects) != 1 || !objects["organizations/other/x"] {
		t.Fatalf("remaining = %v", objects)
	}
}
