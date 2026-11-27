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
// -o file. With STUB_FAIL set, it writes the full body before returning a
// curl error so that a matching checksum cannot hide an ignored exit status.
// With STUB_MKDIR set, it creates an obstructing directory with a sentinel
// before writing the download.
const stubCurl = `#!/usr/bin/env bash
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
  esac
  shift
done
echo call >> "$STUB_CALLS"
if [ -n "${STUB_MKDIR:-}" ]; then
  mkdir -p "$STUB_MKDIR" || exit $?
  printf keep > "$STUB_MKDIR/sentinel" || exit $?
fi
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
cp "$STUB_BODY" "$out" || exit $?
if [ -n "${STUB_FAIL:-}" ]; then
  exit 56
fi
`

// stubSignalMove runs the native mv, then signals the helper before it can
// handle the publication result. The marker shows that mv returned.
const stubSignalMove = `#!/usr/bin/env bash
status=0
"$STUB_REAL_MV" "$@" || status=$?
: > "$STUB_MOVE_DONE"
kill -s "$STUB_SIGNAL" "$PPID"
exit "$status"
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

// seed publishes data as a world-readable archive, as an earlier build does.
func (f *fill) seed(data []byte) {
	f.t.Helper()
	f.seedMode(data, 0o644)
}

// seedMode writes the archive with exactly mode. The explicit chmod defeats
// the process umask, which would otherwise narrow the mode WriteFile applies.
func (f *fill) seedMode(data []byte, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(f.cache, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.archive(), data, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.archive(), mode); err != nil {
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

// requireArchive checks that the archive holds exactly want and that the
// cache directory holds nothing else (no temporary file is left). It does not
// check the mode: a reused archive keeps whatever mode it was seeded with.
func (f *fill) requireArchive(want []byte) {
	f.t.Helper()
	got, err := os.ReadFile(f.archive())
	if err != nil {
		f.t.Fatalf("read the published archive: %v", err)
	}
	if string(got) != string(want) {
		f.t.Fatalf("the published archive holds %q, want %q", got, want)
	}
	f.requireNoLeftovers()
}

// requirePublishedArchive checks requireArchive and that the helper published
// the archive world readable, which only a fresh publish has to guarantee.
func (f *fill) requirePublishedArchive(want []byte) {
	f.t.Helper()
	f.requireArchive(want)
	info, err := os.Stat(f.archive())
	if err != nil {
		f.t.Fatal(err)
	}
	if info.Mode().Perm()&0o444 != 0o444 {
		f.t.Fatalf("the published archive has mode %v, want it world readable", info.Mode().Perm())
	}
}

// requireArchiveMode checks that the archive still has exactly mode.
func (f *fill) requireArchiveMode(mode os.FileMode) {
	f.t.Helper()
	info, err := os.Stat(f.archive())
	if err != nil {
		f.t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		f.t.Fatalf("the archive has mode %v, want %v", info.Mode().Perm(), mode)
	}
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

// The checksum command prints a matching digest but reports an error.
func writeFailingSHA256Sum(t *testing.T, stub, digest string) {
	t.Helper()
	script := "#!/usr/bin/env bash\nprintf '%s  -\\n' '" + digest + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(stub, "sha256sum"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestColdCacheDownloadsAndPublishesOnce(t *testing.T) {
	f := newFill(t)
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requirePublishedArchive(goodBody)
	if got := f.callCount(); got != 1 {
		t.Fatalf("downloader calls = %d, want 1", got)
	}
}

func TestValidCachedArchiveIsReusedWithoutDownload(t *testing.T) {
	f := newFill(t)
	// A cache hit never rewrites the archive, so it keeps a restrictive mode.
	f.seedMode(goodBody, 0o600)
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requireArchive(goodBody)
	f.requireArchiveMode(0o600)
	if got := f.callCount(); got != 0 {
		t.Fatalf("downloader calls = %d, want 0", got)
	}
}

func TestCachePathWithNewlineAcceptsUppercaseDigest(t *testing.T) {
	for _, test := range []struct {
		name      string
		warm      bool
		wantCalls int
		wantMode  os.FileMode
	}{
		{name: "warm", warm: true, wantCalls: 0, wantMode: 0o600},
		{name: "cold", wantCalls: 1, wantMode: 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFill(t)
			f.cache = filepath.Join(filepath.Dir(f.cache), "cache\npath")
			f.sum = strings.ToUpper(f.sum)
			if test.warm {
				f.seedMode(goodBody, test.wantMode)
			}
			if out, err := f.run(f.body("good", goodBody)); err != nil {
				t.Fatalf("fill: %v\n%s", err, out)
			}
			f.requireArchive(goodBody)
			f.requireArchiveMode(test.wantMode)
			if got := f.callCount(); got != test.wantCalls {
				t.Fatalf("downloader calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestCachePathWithNewlinePreservesArchiveAfterVerificationFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     []byte
		hashFail bool
	}{
		{name: "digest mismatch", body: []byte("wrong bytes")},
		{name: "hash command fails", body: goodBody, hashFail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFill(t)
			f.cache = filepath.Join(filepath.Dir(f.cache), "cache\npath")
			older := []byte("a truncated archive")
			f.seed(older)
			if test.hashFail {
				writeFailingSHA256Sum(t, f.stub, f.sum)
			}
			out, err := f.run(f.body("fixture", test.body))
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("fill with failed verification: err = %v, want nonzero exit\n%s", err, out)
			}
			f.requireArchive(older)
			if got := f.callCount(); got != 1 {
				t.Fatalf("downloader calls = %d, want 1", got)
			}
		})
	}
}

func TestInvalidCachedArchiveIsReplacedInTheSameRun(t *testing.T) {
	f := newFill(t)
	f.seed([]byte("a truncated archive that an earlier build published"))
	body := f.body("good", goodBody)
	if out, err := f.run(body); err != nil {
		t.Fatalf("fill: %v\n%s", err, out)
	}
	f.requirePublishedArchive(goodBody)
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

func TestDirectoryAtArchivePathIsRejected(t *testing.T) {
	f := newFill(t)
	sentinel := filepath.Join(f.archive(), "sentinel")
	if err := os.MkdirAll(f.archive(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := f.body("good", goodBody)
	out, err := f.run(body)
	if err == nil {
		t.Fatalf("fill succeeded with a directory at the archive path\n%s", out)
	}
	if !strings.Contains(out, f.archive()) || !strings.Contains(out, "is a directory") {
		t.Fatalf("output does not name the directory path:\n%s", out)
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("downloader calls = %d, want 0", got)
	}
	f.requireObstructingDirectory()
}

func (f *fill) requireObstructingDirectory() {
	f.t.Helper()
	entries, err := os.ReadDir(f.archive())
	if err != nil {
		f.t.Fatalf("read the obstructing directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel" {
		f.t.Fatalf("the obstructing directory changed: %v", entries)
	}
	if got, err := os.ReadFile(filepath.Join(f.archive(), "sentinel")); err != nil || string(got) != "keep" {
		f.t.Fatalf("the sentinel changed: %q, %v", got, err)
	}
	f.requireNoLeftovers()
}

// A directory that appears at the archive path after the early check (a
// race with another process) must also fail the publication and must not
// receive the archive. The stub curl creates the directory during the
// download, which is after the early check and before the rename.
func TestDirectoryAppearingBeforePublishIsRejected(t *testing.T) {
	f := newFill(t)
	body := f.body("good", goodBody)
	out, err := f.run(body, "STUB_MKDIR="+f.archive())
	if err == nil {
		t.Fatalf("fill succeeded although a directory took the archive path\n%s", out)
	}
	if !strings.Contains(out, f.archive()) || !strings.Contains(out, "became a directory") {
		t.Fatalf("output does not name the late directory:\n%s", out)
	}
	f.requireObstructingDirectory()
}

func TestInterruptedCachePublication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		signal     string
		code       int
		obstructed bool
	}{
		{"INT/published", "INT", 130, false},
		{"INT/obstructed", "INT", 130, true},
		{"TERM/published", "TERM", 143, false},
		{"TERM/obstructed", "TERM", 143, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFill(t)
			mv, err := exec.LookPath("mv")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.stub, "mv"), []byte(stubSignalMove), 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(f.stub, "move-done")
			env := []string{"STUB_REAL_MV=" + mv, "STUB_MOVE_DONE=" + marker, "STUB_SIGNAL=" + tc.signal}
			if tc.obstructed {
				env = append(env, "STUB_MKDIR="+f.archive())
			}

			out, err := f.run(f.body("good", goodBody), env...)
			if _, statErr := os.Stat(marker); statErr != nil {
				t.Fatalf("native mv did not return: %v\n%s", statErr, out)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
				t.Fatalf("fill exit = %v, want status %d\n%s", err, tc.code, out)
			}
			if tc.obstructed {
				f.requireObstructingDirectory()
			} else {
				f.requireArchive(goodBody)
				f.requireArchiveMode(0o644)
			}
		})
	}
}

func TestFailedDownloadLeavesTheCacheUntouched(t *testing.T) {
	f := newFill(t)
	stale := []byte("a stale archive")
	f.seed(stale)
	body := f.body("good", goodBody)
	out, err := f.run(body, "STUB_FAIL=1")
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("want a nonzero exit after curl fails with a complete body, got %v\n%s", err, out)
	}
	f.requireArchive(stale)
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
	f.requirePublishedArchive(goodBody)
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
	f.requirePublishedArchive(goodBody)
}
