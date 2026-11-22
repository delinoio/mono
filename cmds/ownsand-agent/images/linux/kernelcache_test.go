// SPDX-License-Identifier: FSL-1.1-ALv2

// Package linux holds the tests of the Linux guest image build scripts that
// run without Docker. The kernel source cache helper runs against a stub
// curl on PATH, so no test uses the network. The tests need bash and GNU
// sha256sum and skip without them (macOS has shasum only) and on Windows;
// the helper itself runs inside the Debian build container. The root file
// system script runs against a stub busybox in build-rootfs_test.go.
package linux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// goodBody is the content of the pinned archive.
var goodBody = []byte("the pinned kernel source")

const (
	kernelVersion = "1.2.3"
	kernelURL     = "https://downloads.invalid/linux-1.2.3.tar.xz"
)

// stubCurl is a curl replacement. It records one line per call in
// $STUB_CALLS, optionally waits until two stubs run at the same time
// ($STUB_BARRIER: each creates a marker file and waits for the second one,
// so the overlap does not depend on timing), then writes $STUB_BODY to the
// -o file. With STUB_PARTIAL set it writes only the first bytes and fails
// with the curl exit status 18 (partial file), like an interrupted download.
const stubCurl = `#!/usr/bin/env bash
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
  esac
  shift
done
echo call >> "$STUB_CALLS"
if [ -n "${STUB_BARRIER:-}" ]; then
  : > "$STUB_BARRIER/started.$$"
  for _ in $(seq 1 600); do
    [ "$(find "$STUB_BARRIER" -name 'started.*' | wc -l)" -ge 2 ] && break
    sleep 0.1
  done
  if [ "$(find "$STUB_BARRIER" -name 'started.*' | wc -l)" -lt 2 ]; then
    echo "the second download did not start" >&2
    exit 1
  fi
fi
if [ -n "${STUB_PARTIAL:-}" ]; then
  head -c 3 "$STUB_BODY" > "$out"
  exit 18
fi
cp "$STUB_BODY" "$out"
`

type fill struct {
	t      *testing.T
	script string
	stub   string // directory with the stub curl
	cache  string
	calls  string
	sum    string // SHA-256 of the pinned archive
}

func newFill(t *testing.T) *fill {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the cache helper runs in a Linux container, not on Windows")
	}
	for _, tool := range []string{"bash", "sha256sum", "mktemp", "seq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	script, err := filepath.Abs("fetch-kernel-source.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "bin")
	if err := os.Mkdir(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stub, "curl"), []byte(stubCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(goodBody)
	return &fill{
		t:      t,
		script: script,
		stub:   stub,
		cache:  filepath.Join(dir, "cache"),
		calls:  filepath.Join(dir, "calls"),
		sum:    hex.EncodeToString(sum[:]),
	}
}

// body writes a download body next to the stub and returns its path.
func (f *fill) body(name string, data []byte) string {
	f.t.Helper()
	path := filepath.Join(f.stub, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fill) archive() string {
	return filepath.Join(f.cache, "linux-"+kernelVersion+".tar.xz")
}

func (f *fill) seed(data []byte) {
	f.t.Helper()
	if err := os.MkdirAll(f.cache, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.archive(), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// command builds one helper run; extra holds more environment variables.
func (f *fill) command(body string, extra ...string) *exec.Cmd {
	cmd := exec.Command("bash", f.script, kernelVersion, f.sum, kernelURL, f.cache)
	cmd.Env = append(os.Environ(),
		"PATH="+f.stub+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_CALLS="+f.calls,
		"STUB_BODY="+body,
	)
	cmd.Env = append(cmd.Env, extra...)
	return cmd
}

func (f *fill) run(body string, extra ...string) (string, error) {
	out, err := f.command(body, extra...).CombinedOutput()
	return string(out), err
}

func (f *fill) callCount() int {
	f.t.Helper()
	data, err := os.ReadFile(f.calls)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Count(string(data), "call\n")
}

// requireArchive checks that the published archive holds exactly want and
// that the cache directory holds nothing else (no temporary file is left).
func (f *fill) requireArchive(want []byte) {
	f.t.Helper()
	got, err := os.ReadFile(f.archive())
	if err != nil {
		f.t.Fatalf("read the published archive: %v", err)
	}
	if string(got) != string(want) {
		f.t.Fatalf("the published archive holds %q, want %q", got, want)
	}
	info, err := os.Stat(f.archive())
	if err != nil {
		f.t.Fatal(err)
	}
	if info.Mode().Perm()&0o444 != 0o444 {
		f.t.Fatalf("the published archive has mode %v, want it world readable", info.Mode().Perm())
	}
	f.requireNoLeftovers()
}

func (f *fill) requireNoLeftovers() {
	f.t.Helper()
	entries, err := os.ReadDir(f.cache)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(f.archive()) {
			f.t.Fatalf("the cache directory holds %q besides the archive", entry.Name())
		}
	}
}

func TestColdCacheDownloadsAndPublishesOnce(t *testing.T) {
	f := newFill(t)
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requireArchive(goodBody)
	if got := f.callCount(); got != 1 {
		t.Fatalf("downloader calls = %d, want 1", got)
	}
}

func TestValidCachedArchiveIsReusedWithoutDownload(t *testing.T) {
	f := newFill(t)
	f.seed(goodBody)
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requireArchive(goodBody)
	if got := f.callCount(); got != 0 {
		t.Fatalf("downloader calls = %d, want 0", got)
	}
}

func TestInvalidCachedArchiveIsReplacedInTheSameRun(t *testing.T) {
	f := newFill(t)
	f.seed([]byte("a truncated archive that an earlier build published"))
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requireArchive(goodBody)
	if got := f.callCount(); got != 1 {
		t.Fatalf("downloader calls = %d, want 1", got)
	}
}

func TestChecksumMismatchPublishesNothing(t *testing.T) {
	f := newFill(t)
	body := f.body("bad", []byte("wrong bytes"))
	out, err := f.run(body)
	if err == nil {
		t.Fatalf("fill succeeded with a wrong download\n%s", out)
	}
	if !strings.Contains(out, "does not match") {
		t.Fatalf("output does not report the mismatch:\n%s", out)
	}
	if _, err := os.Stat(f.archive()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the archive path exists after a failed verification: %v", err)
	}
	f.requireNoLeftovers()
}

func TestInterruptedDownloadLeavesTheCacheUntouched(t *testing.T) {
	f := newFill(t)
	stale := []byte("a stale archive")
	f.seed(stale)
	body := f.body("good", goodBody)
	out, err := f.run(body, "STUB_PARTIAL=1")
	if err == nil {
		t.Fatalf("fill succeeded with an interrupted download\n%s", out)
	}
	got, err := os.ReadFile(f.archive())
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	if string(got) != string(stale) {
		t.Fatalf("a failed download changed the archive to %q", got)
	}
	f.requireNoLeftovers()
}

func TestOverlappingColdFillsBothEndWithTheVerifiedArchive(t *testing.T) {
	f := newFill(t)
	body := f.body("good", goodBody)
	barrier := t.TempDir()
	var wg sync.WaitGroup
	outputs := make([]string, 2)
	errs := make([]error, 2)
	for i := range outputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outputs[i], errs[i] = f.run(body, "STUB_BARRIER="+barrier)
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("fill %d: %v\n%s", i, errs[i], outputs[i])
		}
	}
	f.requireArchive(goodBody)
	if got := f.callCount(); got != 2 {
		t.Fatalf("downloader calls = %d, want 2 (both fills downloaded at once)", got)
	}
}

func TestFailedFillNeverReplacesAnArchiveAnotherFillPublished(t *testing.T) {
	f := newFill(t)
	good := f.body("good", goodBody)
	bad := f.body("bad", []byte("wrong bytes"))
	barrier := t.TempDir()
	var wg sync.WaitGroup
	var goodErr, badErr error
	var goodOut, badOut string
	wg.Add(2)
	go func() {
		defer wg.Done()
		goodOut, goodErr = f.run(good, "STUB_BARRIER="+barrier)
	}()
	go func() {
		defer wg.Done()
		badOut, badErr = f.run(bad, "STUB_BARRIER="+barrier)
	}()
	wg.Wait()
	if goodErr != nil {
		t.Fatalf("the fill with the right download: %v\n%s", goodErr, goodOut)
	}
	if badErr == nil {
		t.Fatalf("the fill with the wrong download succeeded\n%s", badOut)
	}
	f.requireArchive(goodBody)
}
