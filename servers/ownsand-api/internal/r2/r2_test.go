// SPDX-License-Identifier: FSL-1.1-ALv2

package r2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The presigned URL example of the AWS Signature Version 4 documentation
// ("Authenticating Requests: Using Query Parameters").
func TestPresignAWSVector(t *testing.T) {
	c := &Client{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "us-east-1"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := c.presign(http.MethodGet, "examplebucket.s3.amazonaws.com", "https", "/test.txt", 86400*time.Second, at)
	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "X-Amz-Signature="+wantSig) {
		t.Fatalf("presigned URL %s", got)
	}
}

func TestDeletePrefix(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]bool{}
	for i := 1; i <= 3; i++ {
		objects[fmt.Sprintf("organizations/o/snapshots/s/part-%05d", i)] = true
	}
	objects["organizations/o/other"] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=id/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			prefix := r.URL.Query().Get("prefix")
			fmt.Fprint(w, "<ListBucketResult>")
			for k := range objects {
				if strings.HasPrefix(k, prefix) {
					fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
				}
			}
			fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
		case http.MethodDelete:
			delete(objects, strings.TrimPrefix(r.URL.Path, "/bucket/"))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "bucket", AccessKeyID: "id", SecretAccessKey: "secret", HTTP: srv.Client()}
	if err := c.DeletePrefix(context.Background(), "organizations/o/snapshots/s/"); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || !objects["organizations/o/other"] {
		t.Fatalf("objects left: %v", objects)
	}
	if err := c.DeletePrefix(context.Background(), "organizations/o"); err == nil {
		t.Fatal("prefix without a trailing slash accepted")
	}
	u, err := c.Presign(http.MethodPut, PartKey("organizations/o/snapshots/s/", 7), 15*time.Minute)
	if err != nil || !strings.Contains(u, "/bucket/organizations/o/snapshots/s/part-00007?") || !strings.Contains(u, "X-Amz-Expires=900") {
		t.Fatalf("presign: %s %v", u, err)
	}
}
