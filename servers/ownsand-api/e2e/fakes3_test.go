// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeBucket is the bucket of the fake S3 store.
const fakeBucket = "ownsand-e2e"

// fakeS3 is a path-style S3 endpoint in memory: presigned PUT and GET of
// objects, ListObjectsV2 and DELETE. It checks that each request is signed
// (a presigned query or an Authorization header) but does not verify the
// signature; the control plane's signer has its own unit tests.
type fakeS3 struct {
	url string
	mu  sync.Mutex
	// objects maps keys (without the bucket) to their content.
	objects map[string][]byte
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeS3) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") == "" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		http.Error(w, "unsigned request", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != fakeBucket {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPut && key != "":
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objects[key] = data
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && key != "":
		f.mu.Lock()
		data, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		type content struct {
			Key string `xml:"Key"`
		}
		var res struct {
			XMLName     xml.Name  `xml:"ListBucketResult"`
			Contents    []content `xml:"Contents"`
			IsTruncated bool      `xml:"IsTruncated"`
		}
		for _, k := range f.keys(q.Get("prefix")) {
			res.Contents = append(res.Contents, content{Key: k})
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodDelete && key != "":
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}
