// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	// gate, while set, holds every PUT before it stores the object, so a
	// test can interrupt an export mid-transfer. putHeld is signaled once
	// for each held PUT.
	gate    chan struct{}
	putHeld chan struct{}
}

// holdPuts makes every PUT wait until releasePuts or until the client
// gives up (a killed agent); a held PUT stores nothing in that case.
func (f *fakeS3) holdPuts() {
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.putHeld = make(chan struct{}, 64)
	f.mu.Unlock()
}

// releasePuts lets the held PUTs and later PUTs store their objects.
func (f *fakeS3) releasePuts() {
	f.mu.Lock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
	f.mu.Unlock()
}

// waitPutHeld waits until a PUT is held.
func (f *fakeS3) waitPutHeld(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	held := f.putHeld
	f.mu.Unlock()
	select {
	case <-held:
	case <-time.After(60 * time.Second):
		t.Fatal("no part upload reached the object store")
	}
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// sizes returns the size of every object whose key starts with prefix.
func (f *fakeS3) sizes(prefix string) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out[k] = len(v)
		}
	}
	return out
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
		f.mu.Lock()
		gate, held := f.gate, f.putHeld
		f.mu.Unlock()
		if gate != nil {
			held <- struct{}{}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
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
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		var res struct {
			XMLName     xml.Name  `xml:"ListBucketResult"`
			Contents    []content `xml:"Contents"`
			IsTruncated bool      `xml:"IsTruncated"`
		}
		sizes := f.sizes(q.Get("prefix"))
		for _, k := range slices.Sorted(maps.Keys(sizes)) {
			res.Contents = append(res.Contents, content{Key: k, Size: sizes[k]})
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

type fakeS3ListEntry struct {
	Key  string `xml:"Key"`
	Size string `xml:"Size"`
}

func putFakeS3Object(client *http.Client, f *fakeS3, key, body string) error {
	req, err := http.NewRequest(http.MethodPut, f.url+"/"+fakeBucket+"/"+key+"?X-Amz-Signature=test", strings.NewReader(body))
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %s: status %s", key, res.Status)
	}
	return nil
}

func listFakeS3Objects(t *testing.T, ctx context.Context, client *http.Client, f *fakeS3, prefix string) []fakeS3ListEntry {
	t.Helper()
	query := url.Values{"list-type": {"2"}, "prefix": {prefix}, "X-Amz-Signature": {"test"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/"+fakeBucket+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list %q: status %s", prefix, res.Status)
	}
	if got := res.Header.Get("Content-Type"); got != "application/xml" {
		t.Fatalf("list %q: Content-Type = %q, want application/xml", prefix, got)
	}
	var listing struct {
		XMLName     xml.Name          `xml:"ListBucketResult"`
		Contents    []fakeS3ListEntry `xml:"Contents"`
		IsTruncated string            `xml:"IsTruncated"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&listing); err != nil {
		t.Fatalf("list %q: decode XML: %v", prefix, err)
	}
	if listing.IsTruncated != "false" {
		t.Fatalf("list %q: IsTruncated = %q, want false", prefix, listing.IsTruncated)
	}
	return listing.Contents
}

func TestFakeS3ListObjectsV2(t *testing.T) {
	f := newFakeS3(t)
	client := &http.Client{Timeout: 10 * time.Second}
	for _, object := range []struct{ key, body string }{
		{"parts/2", ""},
		{"parts/1", "abc"},
		{"other/1", "x"},
	} {
		if err := putFakeS3Object(client, f, object.key, object.body); err != nil {
			t.Fatal(err)
		}
	}

	want := []fakeS3ListEntry{{Key: "parts/1", Size: "3"}, {Key: "parts/2", Size: "0"}}
	if got := listFakeS3Objects(t, t.Context(), client, f, "parts/"); !slices.Equal(got, want) {
		t.Fatalf("list parts/: entries = %v, want %v", got, want)
	}
}

func TestFakeS3ListObjectsV2ConcurrentPut(t *testing.T) {
	f := newFakeS3(t)
	defer f.releasePuts()
	client := &http.Client{Timeout: 10 * time.Second}

	for round := range 64 {
		prefix := fmt.Sprintf("parts/%d/", round)
		key := prefix + "1"
		f.holdPuts()
		putDone := make(chan error, 1)
		go func() {
			putDone <- putFakeS3Object(client, f, key, "abc")
		}()
		f.waitPutHeld(t)
		// Release the pending PUT as the client sends the listing request.
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { f.releasePuts() },
		})
		got := listFakeS3Objects(t, ctx, client, f, prefix)
		if err := <-putDone; err != nil {
			t.Fatal(err)
		}

		want := []fakeS3ListEntry{{Key: key, Size: "3"}}
		if len(got) != 0 && !slices.Equal(got, want) {
			t.Fatalf("list %q during PUT: entries = %v, want none or %v", prefix, got, want)
		}
		if got := listFakeS3Objects(t, t.Context(), client, f, prefix); !slices.Equal(got, want) {
			t.Fatalf("list %q after PUT: entries = %v, want %v", prefix, got, want)
		}
	}
}
