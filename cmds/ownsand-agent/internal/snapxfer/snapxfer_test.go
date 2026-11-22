// SPDX-License-Identifier: FSL-1.1-ALv2

package snapxfer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestImportRejectsSymlinkEscape imports an archive whose chained symbolic
// links resolve outside the snapshot directory and checks that nothing is
// written there and that the import fails.
func TestImportRejectsSymlinkEscape(t *testing.T) {
	probe := t.TempDir()
	if err := os.Symlink("target", filepath.Join(probe, "probe")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, e := range []struct {
		hdr  tar.Header
		data string
	}{
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "a/b", Typeflag: tar.TypeSymlink, Linkname: ".."}},
		{hdr: tar.Header{Name: "l2", Typeflag: tar.TypeSymlink, Linkname: "a/b/.."}},
		{hdr: tar.Header{Name: "l2/evil", Typeflag: tar.TypeReg, Mode: 0o644}, data: "pwned"},
	} {
		hdr := e.hdr
		hdr.Size = int64(len(e.data))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.data))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b := newBucket(t)
	b.objects["/snap/1"] = archive.Bytes()
	sum := sha256.Sum256(archive.Bytes())
	parts := []*ownsandv1.SnapshotPart{{PartNumber: 1, SizeBytes: int64(archive.Len()), Sha256: sum[:]}}
	tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), TempDir: t.TempDir()}
	base := t.TempDir()
	dst := filepath.Join(base, "content")
	err := tr.Import(context.Background(), "snap", parts, dst)
	if runtime.GOOS == "windows" {
		// Windows removes ".." elements lexically before it follows a link,
		// and os.Root does the same there, so "a/b/.." is "a" and the file
		// is "a/evil" inside the restore root. Import may accept the
		// archive, but then the file must be inside the root.
		if err == nil {
			if b, err := os.ReadFile(filepath.Join(dst, "a", "evil")); err != nil || string(b) != "pwned" {
				t.Fatalf("import accepted the archive, but a/evil is not the file in the restore root: %q, %v", b, err)
			}
		}
	} else if err == nil {
		// On other systems the link "a/b" is resolved first, so "a/b/.."
		// is the parent of the restore root.
		t.Fatal("import accepted an archive that escapes through symbolic links")
	}
	// The staging directory is the parent of the restore root ("content.importing");
	// the escape target is its parent, which is base.
	if _, err := os.Lstat(filepath.Join(base, "evil")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written outside the restore root: %v", err)
	}
	if err != nil {
		if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot content installed after a failed import: %v", err)
		}
	}
	if _, err := os.Lstat(dst + ".importing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory left behind: %v", err)
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

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// listDir returns the entry names of dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// urlLifetime is the lifetime of a signed URL in the control plane.
const urlLifetime = 15 * time.Minute

// clockedBucket is a fake bucket whose signed URLs expire urlLifetime after
// they were issued on a fake clock. Every served request advances the clock
// by step, so a transfer of many parts runs far longer than one lifetime.
// It records the order of URL requests and transfers.
type clockedBucket struct {
	mu      sync.Mutex
	now     time.Time
	step    time.Duration
	objects map[string][]byte
	events  []string
	batches []int
	expired int
	srv     *httptest.Server
}

func newClockedBucket(t *testing.T, step time.Duration) *clockedBucket {
	b := &clockedBucket{now: time.Unix(1_700_000_000, 0), step: step, objects: map[string][]byte{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		issued, _ := strconv.ParseInt(r.URL.Query().Get("issued"), 10, 64)
		if b.now.Sub(time.Unix(issued, 0)) > urlLifetime {
			b.expired++
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		b.events = append(b.events, r.Method+" "+r.URL.Path)
		b.now = b.now.Add(b.step)
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

func (b *clockedBucket) RequestURLs(_ context.Context, snapshotID string, _ ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, len(parts))
	var out []*ownsandv1.SignedPartUrl
	for _, p := range parts {
		b.events = append(b.events, "URL "+strconv.Itoa(int(p)))
		out = append(out, &ownsandv1.SignedPartUrl{PartNumber: p, Url: b.srv.URL + "/" + snapshotID + "/" + strconv.Itoa(int(p)) + "?issued=" + strconv.FormatInt(b.now.Unix(), 10)})
	}
	return out, nil
}

// TestTransferRequestsEachPartURLRightBeforeUse checks that a transfer whose
// parts take three minutes each (far longer than the 15-minute URL lifetime
// over all parts) succeeds, because each part asks for its own URL right
// before it uses it, after it prepared the bytes and the hash.
func TestTransferRequestsEachPartURLRightBeforeUse(t *testing.T) {
	b := newClockedBucket(t, 3*time.Minute)
	tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: quietLogger(), TempDir: t.TempDir()}
	src := t.TempDir()
	content := strings.Repeat("snapshot-", 5000)
	os.WriteFile(filepath.Join(src, "disk"), []byte(content), 0o600)
	parts, _, err := tr.Export(context.Background(), "snap", src, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) <= 8 {
		t.Fatalf("parts = %d, want more than 8", len(parts))
	}
	exportEvents := slices.Clone(b.events)
	dst := filepath.Join(t.TempDir(), "content")
	if err := tr.Import(context.Background(), "snap", parts, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "disk")); string(got) != content {
		t.Fatal("imported content differs")
	}
	if b.expired != 0 {
		t.Fatalf("%d requests used an expired URL", b.expired)
	}
	for _, n := range b.batches {
		if n != 1 {
			t.Fatalf("a URL request named %d parts, want 1", n)
		}
	}
	// Each URL request is followed by exactly the transfer of its part
	// before the next URL request.
	var want []string
	for p := 1; p <= len(parts); p++ {
		want = append(want, "URL "+strconv.Itoa(p), "PUT /snap/"+strconv.Itoa(p))
	}
	if !slices.Equal(exportEvents, want) {
		t.Fatalf("export events = %v, want %v", exportEvents, want)
	}
	var wantImport []string
	for p := 1; p <= len(parts); p++ {
		wantImport = append(wantImport, "URL "+strconv.Itoa(p), "GET /snap/"+strconv.Itoa(p))
	}
	if got := b.events[len(exportEvents):]; !slices.Equal(got, wantImport) {
		t.Fatalf("import events = %v, want %v", got, wantImport)
	}
}

// storeSnapshot exports content into a plain bucket and returns the bucket,
// the parts and the object paths of the parts.
func storeSnapshot(t *testing.T, content string, partSize int64) (*bucket, []*ownsandv1.SnapshotPart) {
	t.Helper()
	b := newBucket(t)
	tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: quietLogger(), TempDir: t.TempDir()}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "disk"), []byte(content), 0o600)
	parts, _, err := tr.Export(context.Background(), "snap", src, partSize)
	if err != nil {
		t.Fatal(err)
	}
	return b, parts
}

// TestImportRejectsACorruptDownload checks that a part that is short, longer
// than its size or different in one byte fails the import, leaves no content
// and no staging directory, and removes the temporary archive.
func TestImportRejectsACorruptDownload(t *testing.T) {
	content := strings.Repeat("snapshot-", 2000)
	cases := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"missing byte", func(d []byte) []byte { return d[:len(d)-1] }},
		{"appended byte", func(d []byte) []byte { return append(slices.Clone(d), 0) }},
		{"flipped byte", func(d []byte) []byte { d = slices.Clone(d); d[len(d)/2] ^= 1; return d }},
		{"empty", func([]byte) []byte { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, parts := storeSnapshot(t, content, 4096)
			last := "/snap/" + strconv.Itoa(len(parts))
			b.objects[last] = tc.mutate(b.objects[last])
			tempDir := t.TempDir()
			tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: quietLogger(), TempDir: tempDir}
			base := t.TempDir()
			dst := filepath.Join(base, "content")
			err := tr.Import(context.Background(), "snap", parts, dst)
			if err == nil {
				t.Fatal("corrupt download accepted")
			}
			if strings.Contains(err.Error(), "?sig=") || strings.Contains(err.Error(), b.srv.URL) {
				t.Fatalf("error leaks the signed URL: %v", err)
			}
			if names := listDir(t, base); len(names) != 0 {
				t.Fatalf("import left %v", names)
			}
			if names := listDir(t, tempDir); len(names) != 0 {
				t.Fatalf("import left temporary files %v", names)
			}
		})
	}
}

// TestImportRejectsATruncatedBody checks a response that promises more bytes
// than it sends.
func TestImportRejectsATruncatedBody(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 10000)
	sum := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data[:100])
		// Closing the connection before the promised length is an
		// interrupted download.
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	urls := fixedURLs{url: srv.URL + "/part?sig=secret"}
	tempDir := t.TempDir()
	tr := &Transfer{HTTP: srv.Client(), URLs: urls, Logger: quietLogger(), TempDir: tempDir}
	parts := []*ownsandv1.SnapshotPart{{PartNumber: 1, SizeBytes: int64(len(data)), Sha256: sum[:]}}
	err := tr.Import(context.Background(), "snap", parts, filepath.Join(t.TempDir(), "content"))
	if err == nil || strings.Contains(err.Error(), "sig=") {
		t.Fatalf("err = %v, want a sanitized failure", err)
	}
	if names := listDir(t, tempDir); len(names) != 0 {
		t.Fatalf("import left temporary files %v", names)
	}
}

type fixedURLs struct{ url string }

func (f fixedURLs) RequestURLs(_ context.Context, _ string, _ ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	var out []*ownsandv1.SignedPartUrl
	for _, p := range parts {
		out = append(out, &ownsandv1.SignedPartUrl{PartNumber: p, Url: f.url})
	}
	return out, nil
}

// hookURLs wraps a URL source and runs hook before it answers.
type hookURLs struct {
	URLSource
	hook func(call int)
	n    int
}

func (h *hookURLs) RequestURLs(ctx context.Context, id string, op ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	h.n++
	h.hook(h.n)
	return h.URLSource.RequestURLs(ctx, id, op, parts)
}

func TestExportStopsWhenTheContextIsDone(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "disk"), []byte(strings.Repeat("snapshot-", 5000)), 0o600)
	t.Run("before packing", func(t *testing.T) {
		urls := &countingURLs{}
		tempDir := t.TempDir()
		tr := &Transfer{HTTP: http.DefaultClient, URLs: urls, Logger: quietLogger(), TempDir: tempDir}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := tr.Export(ctx, "snap", src, 4096); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if urls.calls != 0 {
			t.Fatalf("URL requests = %d, want 0", urls.calls)
		}
		if names := listDir(t, tempDir); len(names) != 0 {
			t.Fatalf("export left %v", names)
		}
	})
	t.Run("between parts", func(t *testing.T) {
		b := newBucket(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		urls := &hookURLs{URLSource: b, hook: func(call int) {
			if call == 3 {
				cancel()
			}
		}}
		tempDir := t.TempDir()
		tr := &Transfer{HTTP: b.srv.Client(), URLs: urls, Logger: quietLogger(), TempDir: tempDir}
		if _, _, err := tr.Export(ctx, "snap", src, 4096); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(b.objects) != 2 {
			t.Fatalf("%d parts uploaded, want 2 (the third URL request cancelled the export)", len(b.objects))
		}
		if names := listDir(t, tempDir); len(names) != 0 {
			t.Fatalf("export left %v", names)
		}
	})
}

func TestImportStopsWhenTheContextIsDone(t *testing.T) {
	b, parts := storeSnapshot(t, strings.Repeat("snapshot-", 5000), 4096)
	cases := []struct {
		name string
		run  func(cancel context.CancelFunc) *Transfer
	}{
		{"during the download", func(cancel context.CancelFunc) *Transfer {
			urls := &hookURLs{URLSource: b, hook: func(call int) {
				if call == 3 {
					cancel()
				}
			}}
			return &Transfer{HTTP: b.srv.Client(), URLs: urls, Logger: quietLogger()}
		}},
		{"after the extraction, before the publication", func(cancel context.CancelFunc) *Transfer {
			return &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: quietLogger(), afterExtract: cancel}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := tc.run(cancel)
			tr.TempDir = t.TempDir()
			base := t.TempDir()
			err := tr.Import(ctx, "snap", parts, filepath.Join(base, "content"))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if names := listDir(t, base); len(names) != 0 {
				t.Fatalf("import left %v", names)
			}
			if names := listDir(t, tr.TempDir); len(names) != 0 {
				t.Fatalf("import left temporary files %v", names)
			}
		})
	}
}

// diskBucket is a fake bucket that keeps every object in a file, so a test
// of large parts holds no part in memory on the server side.
func newDiskBucket(t *testing.T, dir string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/"), "/", "_"))
		switch r.Method {
		case http.MethodPut:
			f, err := os.Create(path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, err = io.Copy(f, r.Body)
			f.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		case http.MethodGet:
			http.ServeFile(w, r, path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type pathURLs struct{ base string }

func (p pathURLs) RequestURLs(_ context.Context, id string, _ ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	var out []*ownsandv1.SignedPartUrl
	for _, n := range parts {
		out = append(out, &ownsandv1.SignedPartUrl{PartNumber: n, Url: p.base + "/" + id + "/" + strconv.Itoa(int(n))})
	}
	return out, nil
}

// TestLargePartTransferUsesBoundedMemory exports and imports one part that is
// far larger than the memory budget and checks that the allocations of the
// whole process stay a small fraction of the part size. A transfer that
// materializes the part allocates at least its size.
func TestLargePartTransferUsesBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes about 150 MiB of temporary files")
	}
	const partSize = 32 << 20
	const budget = 8 << 20
	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "disk"))
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file: the pattern in the middle makes the hash depend on position.
	if err := f.Truncate(partSize - 4096); err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("middle"), partSize/2)
	f.Close()
	srv := newDiskBucket(t, t.TempDir())
	tr := &Transfer{HTTP: srv.Client(), URLs: pathURLs{base: srv.URL}, Logger: quietLogger(), TempDir: t.TempDir()}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	parts, size, err := tr.Export(context.Background(), "snap", src, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "content")
	if err := tr.Import(context.Background(), "snap", parts, dst); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if len(parts) != 1 || size < partSize-4096 {
		t.Fatalf("parts %d, size %d: the test needs one large part", len(parts), size)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > budget {
		t.Fatalf("a %d byte part allocated %d bytes, budget %d", size, grown, budget)
	}
	info, err := os.Stat(filepath.Join(dst, "disk"))
	if err != nil || info.Size() != partSize-4096 {
		t.Fatalf("imported file: %v %v", info, err)
	}
}

// TestPutSendsAnExactLengthReplayableBody checks the upload request: exact
// Content-Length, the bytes of the archive and a body that can be replayed.
func TestPutSendsAnExactLengthReplayableBody(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 1000))
	f, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(append([]byte("prefix"), data...))
	var got []byte
	var length int64
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			// A 307 makes the client replay the body through GetBody.
			http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
			redirected = true
			return
		}
		length = r.ContentLength
		got, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	tr := &Transfer{HTTP: srv.Client(), Logger: quietLogger()}
	if err := tr.put(context.Background(), srv.URL+"/first", f, 6, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if !redirected || length != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("redirected=%v length=%d body ok=%v", redirected, length, bytes.Equal(got, data))
	}
}
