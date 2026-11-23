// SPDX-License-Identifier: FSL-1.1-ALv2

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

// fetchKernelBody is the content of the fixture kernel.
var fetchKernelBody = []byte("the pinned guest kernel")

// fetchKernelStubCurl is a curl replacement. It appends the -o path of each
// call to $STUB_OUTS, optionally waits until two stubs run at the same time
// ($STUB_BARRIER: each creates a marker file and waits for the second one, so
// the overlap does not depend on timing), then writes $STUB_BODY to the -o
// file. With STUB_PARTIAL set it writes only the first bytes and fails with
// the curl exit status 18 (partial file), like an interrupted download.
const fetchKernelStubCurl = `#!/usr/bin/env bash
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
  esac
  shift
done
echo "$out" >> "$STUB_OUTS"
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

// fetchKernelStubUname reports the architecture in $STUB_ARCH, so the tests
// also run on a host that is not x86_64.
const fetchKernelStubUname = `#!/usr/bin/env bash
echo "${STUB_ARCH:-x86_64}"
`

type kernelFetch struct {
	t       *testing.T
	script  string // temporary copy of fetch-kernel.sh pinned to the fixture
	stub    string // directory with the stub curl and uname
	outDir  string
	outputs string // file that records the -o path of every download
	sum     string // SHA-256 of the fixture kernel
}

// newKernelFetch copies fetch-kernel.sh and replaces its pinned URL and
// digest lines with the fixture values, so the production script needs no
// test hook. The test fails when the script no longer has these two lines.
func newKernelFetch(t *testing.T) *kernelFetch {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fetch script runs in a Linux container, not on Windows")
	}
	for _, tool := range []string{"bash", "sha256sum", "mktemp", "seq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	original, err := os.ReadFile("fetch-kernel.sh")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fetchKernelBody)
	digest := hex.EncodeToString(sum[:])
	replaced := 0
	lines := strings.Split(string(original), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "url="):
			lines[i] = `url="https://downloads.invalid/vmlinux"`
			replaced++
		case strings.HasPrefix(line, "sha256="):
			lines[i] = `sha256="` + digest + `"`
			replaced++
		}
	}
	if replaced != 2 {
		t.Fatalf("fetch-kernel.sh has %d pinned url/sha256 lines, want 2", replaced)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fetch-kernel.sh")
	if err := os.WriteFile(script, []byte(strings.Join(lines, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "bin")
	if err := os.Mkdir(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"curl": fetchKernelStubCurl, "uname": fetchKernelStubUname} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outDir := filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &kernelFetch{
		t:       t,
		script:  script,
		stub:    stub,
		outDir:  outDir,
		outputs: filepath.Join(dir, "outputs"),
		sum:     digest,
	}
}

func (k *kernelFetch) kernel() string { return filepath.Join(k.outDir, "vmlinux") }

// body writes a download body next to the stub and returns its path.
func (k *kernelFetch) body(name string, data []byte) string {
	k.t.Helper()
	path := filepath.Join(k.stub, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		k.t.Fatal(err)
	}
	return path
}

func (k *kernelFetch) seed(data []byte) {
	k.t.Helper()
	if err := os.WriteFile(k.kernel(), data, 0o644); err != nil {
		k.t.Fatal(err)
	}
}

// run starts one fetch; extra holds more environment variables.
func (k *kernelFetch) run(body string, extra ...string) (string, error) {
	cmd := exec.Command("bash", k.script, k.kernel())
	cmd.Env = append(os.Environ(),
		"PATH="+k.stub+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_OUTS="+k.outputs,
		"STUB_BODY="+body,
	)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// downloads returns the staging path of each download in start order.
func (k *kernelFetch) downloads() []string {
	k.t.Helper()
	data, err := os.ReadFile(k.outputs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		k.t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// requireKernel checks that the published kernel holds exactly want, is
// world readable and that the output directory holds nothing else (no
// staging file is left).
func (k *kernelFetch) requireKernel(want []byte) {
	k.t.Helper()
	got, err := os.ReadFile(k.kernel())
	if err != nil {
		k.t.Fatalf("read the published kernel: %v", err)
	}
	if string(got) != string(want) {
		k.t.Fatalf("the published kernel holds %q, want %q", got, want)
	}
	info, err := os.Stat(k.kernel())
	if err != nil {
		k.t.Fatal(err)
	}
	if info.Mode().Perm()&0o444 != 0o444 {
		k.t.Fatalf("the published kernel has mode %v, want it world readable", info.Mode().Perm())
	}
	k.requireNoLeftovers()
}

func (k *kernelFetch) requireNoLeftovers() {
	k.t.Helper()
	entries, err := os.ReadDir(k.outDir)
	if err != nil {
		k.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(k.kernel()) {
			k.t.Fatalf("the output directory holds %q besides the kernel", entry.Name())
		}
	}
}

func TestFetchKernelSinglePublishesReadableKernel(t *testing.T) {
	k := newKernelFetch(t)
	body := k.body("good", fetchKernelBody)
	if out, err := k.run(body); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	k.requireKernel(fetchKernelBody)
	if got := k.downloads(); len(got) != 1 {
		t.Fatalf("downloads = %v, want 1", got)
	}
}

func TestFetchKernelReplacesAnOlderKernelOnSuccess(t *testing.T) {
	k := newKernelFetch(t)
	k.seed([]byte("an older kernel"))
	body := k.body("good", fetchKernelBody)
	if out, err := k.run(body); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	k.requireKernel(fetchKernelBody)
}

func TestFetchKernelOverlappingFetchesUseOwnStagingFiles(t *testing.T) {
	k := newKernelFetch(t)
	body := k.body("good", fetchKernelBody)
	barrier := t.TempDir()
	var wg sync.WaitGroup
	outputs := make([]string, 2)
	errs := make([]error, 2)
	for i := range outputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outputs[i], errs[i] = k.run(body, "STUB_BARRIER="+barrier)
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("fetch %d: %v\n%s", i, errs[i], outputs[i])
		}
	}
	k.requireKernel(fetchKernelBody)
	staging := k.downloads()
	if len(staging) != 2 {
		t.Fatalf("downloads = %v, want 2 (both fetches downloaded at once)", staging)
	}
	if staging[0] == staging[1] {
		t.Fatalf("both fetches downloaded into the same file %q", staging[0])
	}
	for _, path := range staging {
		if filepath.Dir(path) != k.outDir || !strings.HasPrefix(filepath.Base(path), "vmlinux.download.") {
			t.Fatalf("staging file %q is not a temporary file next to the output", path)
		}
	}
}

// While a fetch with the right download overlaps one with a wrong download,
// the wrong one must fail without touching what the other one publishes.
func TestFetchKernelFailedFetchNeverReplacesAKernelAnotherFetchPublished(t *testing.T) {
	k := newKernelFetch(t)
	good := k.body("good", fetchKernelBody)
	bad := k.body("bad", []byte("wrong bytes"))
	barrier := t.TempDir()
	var wg sync.WaitGroup
	var goodErr, badErr error
	var goodOut, badOut string
	wg.Add(2)
	go func() {
		defer wg.Done()
		goodOut, goodErr = k.run(good, "STUB_BARRIER="+barrier)
	}()
	go func() {
		defer wg.Done()
		badOut, badErr = k.run(bad, "STUB_BARRIER="+barrier)
	}()
	wg.Wait()
	if goodErr != nil {
		t.Fatalf("the fetch with the right download: %v\n%s", goodErr, goodOut)
	}
	if badErr == nil {
		t.Fatalf("the fetch with the wrong download succeeded\n%s", badOut)
	}
	k.requireKernel(fetchKernelBody)
}

func TestFetchKernelChecksumMismatchLeavesAnExistingKernel(t *testing.T) {
	k := newKernelFetch(t)
	older := []byte("an older kernel")
	k.seed(older)
	body := k.body("bad", []byte("wrong bytes"))
	out, err := k.run(body)
	if err == nil {
		t.Fatalf("fetch succeeded with a wrong download\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("fetch did not run to an exit status: %T", err)
	}
	k.requireKernel(older)
}

func TestFetchKernelChecksumMismatchPublishesNothing(t *testing.T) {
	k := newKernelFetch(t)
	body := k.body("bad", []byte("wrong bytes"))
	if out, err := k.run(body); err == nil {
		t.Fatalf("fetch succeeded with a wrong download\n%s", out)
	}
	if _, err := os.Stat(k.kernel()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the kernel path exists after a failed verification: %v", err)
	}
	k.requireNoLeftovers()
}

func TestFetchKernelFailedDownloadLeavesAnExistingKernel(t *testing.T) {
	k := newKernelFetch(t)
	older := []byte("an older kernel")
	k.seed(older)
	body := k.body("good", fetchKernelBody)
	if out, err := k.run(body, "STUB_PARTIAL=1"); err == nil {
		t.Fatalf("fetch succeeded with an interrupted download\n%s", out)
	}
	k.requireKernel(older)
}

func TestFetchKernelRejectsOtherArchitecturesBeforeDownloading(t *testing.T) {
	k := newKernelFetch(t)
	body := k.body("good", fetchKernelBody)
	out, err := k.run(body, "STUB_ARCH=aarch64")
	if err == nil {
		t.Fatalf("fetch succeeded on a non-x86_64 host\n%s", out)
	}
	if !strings.Contains(out, "only the x86_64 kernel is pinned") {
		t.Fatalf("output does not report the architecture:\n%s", out)
	}
	if got := k.downloads(); len(got) != 0 {
		t.Fatalf("downloads = %v, want none", got)
	}
	k.requireNoLeftovers()
}

func TestFetchKernelRequiresExactlyOneArgument(t *testing.T) {
	k := newKernelFetch(t)
	cmd := exec.Command("bash", k.script)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("fetch without an argument: err = %T, want exit status 2\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage:") {
		t.Fatalf("output has no usage line:\n%s", out)
	}
}
