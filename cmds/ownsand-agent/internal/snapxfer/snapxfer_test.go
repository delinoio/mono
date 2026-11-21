// SPDX-License-Identifier: Apache-2.0

package snapxfer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// bucket is a fake R2 bucket with signed URLs.
type bucket struct {
	mu       sync.Mutex
	objects  map[string][]byte
	srv      *httptest.Server
	requests int
}

func newBucket(t *testing.T) *bucket {
	b := &bucket{objects: map[string][]byte{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			b.objects[r.URL.Path] = data
		case http.MethodGet:
			data, ok := b.objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bucket) RequestURLs(_ context.Context, snapshotID string, op ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	b.mu.Lock()
	b.requests++
	b.mu.Unlock()
	var out []*ownsandv1.SignedPartUrl
	for _, p := range parts {
		out = append(out, &ownsandv1.SignedPartUrl{PartNumber: p, Url: b.srv.URL + "/" + snapshotID + "/" + strconv.Itoa(int(p)) + "?sig=secret"})
	}
	return out, nil
}

func TestExportImportRoundTrip(t *testing.T) {
	b := newBucket(t)
	tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), TempDir: t.TempDir()}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "disk"), []byte(strings.Repeat("snapshot-", 5000)), 0o600)
	parts, size, err := tr.Export(context.Background(), "snap", src, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 10 || size <= 0 || b.requests < 2 {
		t.Fatalf("parts %d size %d url requests %d", len(parts), size, b.requests)
	}
	dst := filepath.Join(t.TempDir(), "content")
	if err := tr.Import(context.Background(), "snap", parts, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "disk"))
	if string(got) != strings.Repeat("snapshot-", 5000) {
		t.Fatal("imported content differs")
	}
	// A part whose hash differs from the command is rejected.
	parts[2].Sha256[0] ^= 1
	if err := tr.Import(context.Background(), "snap", parts, filepath.Join(t.TempDir(), "c2")); err == nil {
		t.Fatal("tampered hash accepted")
	}
}
