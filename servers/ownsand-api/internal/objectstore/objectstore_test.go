// SPDX-License-Identifier: FSL-1.1-ALv2

package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	var mu sync.Mutex
	objects := map[string]bool{}
	for i := 0; i < 3; i++ {
		objects[fmt.Sprintf("organizations/o/snapshots/s/parts/%d", i+1)] = true
	}
	objects["organizations/other/x"] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=k/") || req.Header.Get("x-amz-date") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case http.MethodGet:
			prefix := req.URL.Query().Get("prefix")
			var b strings.Builder
			b.WriteString("<ListBucketResult>")
			// Return one key per page to exercise continuation.
			for k := range objects {
				if strings.HasPrefix(k, prefix) {
					fmt.Fprintf(&b, "<Contents><Key>%s</Key></Contents><IsTruncated>true</IsTruncated><NextContinuationToken>t</NextContinuationToken>", k)
					break
				}
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
	if err := r.DeletePrefix(context.Background(), "organizations/o/"); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || !objects["organizations/other/x"] {
		t.Fatalf("remaining = %v", objects)
	}
}
