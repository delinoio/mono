// SPDX-License-Identifier: FSL-1.1-ALv2

package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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

// TestPresignExpiryMatchesSignedValues checks that the returned expiry equals
// the X-Amz-Date plus X-Amz-Expires encoded in the URL when the signing clock
// has a fractional second.
func TestPresignExpiryMatchesSignedValues(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 900_000_000, time.UTC)
	r := &R2{
		Endpoint:        "https://acct.r2.cloudflarestorage.com",
		Bucket:          "snapshots",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		Now:             func() time.Time { return now },
	}
	for _, tc := range []struct {
		name string
		sign func(key string, ttl time.Duration) (string, time.Time, error)
	}{
		{"put", r.PresignPut},
		{"get", r.PresignGet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signed, exp, err := tc.sign("organizations/o/snapshots/s/parts/1", 15*time.Minute+500*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(signed)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			date, err := time.Parse(amzDateFormat, q.Get("X-Amz-Date"))
			if err != nil {
				t.Fatal(err)
			}
			seconds, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			want := date.Add(time.Duration(seconds) * time.Second)
			if !exp.Equal(want) {
				t.Fatalf("expiry = %v, signed date plus expires = %v", exp, want)
			}
			if want := time.Date(2026, 1, 2, 12, 15, 0, 0, time.UTC); !exp.Equal(want) {
				t.Fatalf("expiry = %v, want %v", exp, want)
			}
		})
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

// TestDeletePrefixRejectsIncompleteListing checks that DeletePrefix returns
// an error and deletes nothing when a successful (HTTP 200) listing is
// truncated and has an omitted or empty continuation token. Returning nil
// there would let the caller finish a cleanup job while objects under the
// prefix remain. The fixture holds two keys under the prefix but serves only
// the first one.
func TestDeletePrefixRejectsIncompleteListing(t *testing.T) {
	const prefix = "organizations/o/"
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "omitted token", token: ""},
		{name: "empty token", token: "<NextContinuationToken></NextContinuationToken>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lists, deletes int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch req.Method {
				case http.MethodGet:
					lists++
					_, _ = w.Write([]byte("<ListBucketResult><Contents><Key>" + prefix + "1</Key></Contents>" +
						"<IsTruncated>true</IsTruncated>" + tc.token + "</ListBucketResult>"))
				case http.MethodDelete:
					deletes++
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			r := &R2{Endpoint: srv.URL, Bucket: "bucket", AccessKeyID: "k", SecretAccessKey: "s"}
			if err := r.DeletePrefix(context.Background(), prefix); err == nil {
				t.Fatal("DeletePrefix = nil, want an error for a truncated page without a continuation token")
			}
			mu.Lock()
			defer mu.Unlock()
			if lists != 1 || deletes != 0 {
				t.Fatalf("list requests = %d, delete requests = %d, want 1 and 0", lists, deletes)
			}
		})
	}
}

// listServer serves the ListObjectsV2 pages that pages maps by continuation
// token ("" is the first page) and records every request token.
func listServer(t *testing.T, prefix string, pages map[string]string) (*R2, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		query := req.URL.Query()
		token := query.Get("continuation-token")
		tokens = append(tokens, token)
		body, ok := pages[token]
		if req.Method != http.MethodGet || query.Get("list-type") != "2" || query.Get("prefix") != prefix || !ok || len(tokens) > len(pages) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &R2{Endpoint: srv.URL, Bucket: "bucket", AccessKeyID: "k", SecretAccessKey: "s"}, &tokens
}

// TestListPrefixFollowsContinuationTokens lists two pages and checks that
// every key and its size is returned in order.
func TestListPrefixFollowsContinuationTokens(t *testing.T) {
	const prefix = "organizations/o/snapshots/s/attempts/a/"
	r, tokens := listServer(t, prefix, map[string]string{
		"": `<ListBucketResult><Contents><Key>` + prefix + `1</Key><Size>5242880</Size></Contents>` +
			`<IsTruncated>true</IsTruncated><NextContinuationToken>page-2</NextContinuationToken></ListBucketResult>`,
		"page-2": `<ListBucketResult><Contents><Key>` + prefix + `2</Key><Size>17</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`,
	})
	got, err := r.ListPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []Object{{Key: prefix + "1", Size: 5 << 20}, {Key: prefix + "2", Size: 17}}
	if !slices.Equal(got, want) {
		t.Fatalf("objects = %v, want %v", got, want)
	}
	if !slices.Equal(*tokens, []string{"", "page-2"}) {
		t.Fatalf("continuation tokens = %q", *tokens)
	}
}

// TestListPrefixRejectsIncompleteListing checks that a truncated page without
// a continuation token and a failed request return an error instead of a
// partial list.
func TestListPrefixRejectsIncompleteListing(t *testing.T) {
	const prefix = "p/"
	r, _ := listServer(t, prefix, map[string]string{
		"": `<ListBucketResult><Contents><Key>p/1</Key><Size>1</Size></Contents><IsTruncated>true</IsTruncated></ListBucketResult>`,
	})
	if got, err := r.ListPrefix(context.Background(), prefix); err == nil {
		t.Fatalf("ListPrefix = %v, want an error for a truncated page without a token", got)
	}
	if got, err := r.ListPrefix(context.Background(), "other/"); err == nil {
		t.Fatalf("ListPrefix of an unexpected prefix = %v, want the status error", got)
	}
}

// TestMemoryListPrefix checks the listing of the in-memory store: sorted
// objects of the prefix with their sizes, replacement, the call counter and
// the injected failure.
func TestMemoryListPrefix(t *testing.T) {
	m := NewMemory(nil)
	m.PutObject("a/2", []byte("xy"))
	m.PutSized("a/1", 1<<30)
	m.PutObject("b/1", []byte("z"))
	got, err := m.ListPrefix(context.Background(), "a/")
	if err != nil || !slices.Equal(got, []Object{{Key: "a/1", Size: 1 << 30}, {Key: "a/2", Size: 2}}) {
		t.Fatalf("ListPrefix = (%v, %v)", got, err)
	}
	m.PutSized("a/2", 7)
	m.PutObject("a/1", []byte("q"))
	got, _ = m.ListPrefix(context.Background(), "a/")
	if !slices.Equal(got, []Object{{Key: "a/1", Size: 1}, {Key: "a/2", Size: 7}}) {
		t.Fatalf("after replacement = %v", got)
	}
	if m.ListCalls() != 2 {
		t.Fatalf("ListCalls = %d, want 2", m.ListCalls())
	}
	m.SetListFail(errors.New("boom"))
	if _, err := m.ListPrefix(context.Background(), "a/"); err == nil {
		t.Fatal("ListPrefix succeeded while it was set to fail")
	}
	if err := m.DeletePrefix(context.Background(), "a/"); err != nil {
		t.Fatal(err)
	}
	m.SetListFail(nil)
	if got, _ := m.ListPrefix(context.Background(), "a/"); len(got) != 0 {
		t.Fatalf("after DeletePrefix = %v", got)
	}
}
