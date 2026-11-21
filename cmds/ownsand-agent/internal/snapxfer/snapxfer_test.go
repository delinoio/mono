// SPDX-License-Identifier: FSL-1.1-ALv2

package snapxfer

import (
	"context"
	"errors"
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

func TestPartCountLimit(t *testing.T) {
	const partSize = int64(64 << 20)
	cases := []struct {
		size, want int64
	}{
		{0, 1},
		{1, 1},
		{partSize * maxParts, maxParts},
		{partSize*maxParts + 1, maxParts + 1},
	}
	for _, c := range cases {
		if got := partCount(c.size, partSize); got != c.want {
			t.Errorf("partCount(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

type failTransport struct{ requests int }

func (f *failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.requests++
	return nil, io.ErrUnexpectedEOF
}

type countingURLs struct{ calls int }

func (c *countingURLs) RequestURLs(context.Context, string, ownsandv1.SignedUrlOperation, []int32) ([]*ownsandv1.SignedPartUrl, error) {
	c.calls++
	return nil, io.ErrUnexpectedEOF
}

func TestExportRejectsTooManyPartsBeforeUpload(t *testing.T) {
	dir := t.TempDir()
	// The tar adds 512-byte blocks, so a 12 KiB file exceeds 10,000 bytes.
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 12<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	urls := &countingURLs{}
	rt := &failTransport{}
	tr := &Transfer{HTTP: &http.Client{Transport: rt}, URLs: urls, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TempDir: t.TempDir()}
	_, _, err := tr.Export(context.Background(), "snap", dir, 1)
	if !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("err = %v, want ErrTooManyParts", err)
	}
	var tooMany *TooManyPartsError
	if !errors.As(err, &tooMany) || tooMany.Limit != maxParts || tooMany.PartCount <= maxParts {
		t.Fatalf("unexpected error details: %+v", tooMany)
	}
	if urls.calls != 0 || rt.requests != 0 {
		t.Fatalf("URL requests = %d, HTTP requests = %d, want 0", urls.calls, rt.requests)
	}
}
