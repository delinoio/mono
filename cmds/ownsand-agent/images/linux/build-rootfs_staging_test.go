// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const stubApplets = "STUB_APPLETS=sh\nls\nbusybox\ninit"

// runSized runs the script with an explicit image size argument.
func (b *rootfsBuild) runSized(size string) (string, error) {
	cmd := exec.Command("bash", b.script, b.out, size)
	cmd.Env = append(append(os.Environ(), b.env...), stubApplets)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireMkfs skips the test when no mkfs.ext4 is installed.
func requireMkfs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, statErr := os.Stat("/usr/sbin/mkfs.ext4"); statErr != nil {
			t.Skip("mkfs.ext4 is not available")
		}
	}
}

// requireOutputDirEntries checks that the output directory holds exactly
// the wanted entries, so a staged file left beside the output fails the test.
func (b *rootfsBuild) requireOutputDirEntries(want ...string) {
	b.t.Helper()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	// The harness directory always holds its stubs, guest and scratch.
	for _, name := range []string{"bin", "guest", "tmp"} {
		delete(got, name)
	}
	if len(got) != len(want) {
		b.t.Fatalf("output directory entries = %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			b.t.Fatalf("output directory entries = %v, want %v", got, want)
		}
	}
}

var rollbackTools = [...]string{"truncate", "mkfs.ext4"}

// Log each tool only after it sees a regular staged file. In the formatting
// case, log truncate after the real command has successfully allocated it.
const stubRollbackTool = `#!/usr/bin/env bash
set -eu
tool="${0##*/}"
staged="${!#}"
if [[ ! -f "$staged" || -L "$staged" ]]; then
  printf '%s: expected a regular staged file at %s\n' "$tool" "$staged" >&2
  exit 1
fi
if [[ "$tool" == truncate && "$tool" != "$STUB_FAILURE_TOOL" ]]; then
  "$STUB_REAL_TRUNCATE" "$@"
fi
printf '%s\t%s\n' "$tool" "$staged" >> "$STUB_TOOL_LOG"
if [[ "$tool" == "$STUB_FAILURE_TOOL" ]]; then
  exit "$STUB_FAILURE_STATUS"
fi
`

func (b *rootfsBuild) requireToolFailure(failTool string) {
	b.t.Helper()
	const failureStatus = 42
	realTruncate, err := exec.LookPath("truncate")
	if err != nil {
		b.t.Fatal(err)
	}
	bin := filepath.Join(b.dir, "bin")
	for _, tool := range rollbackTools {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stubRollbackTool), 0o755); err != nil {
			b.t.Fatal(err)
		}
	}
	logPath := filepath.Join(bin, "rollback-tools.log")
	out, runErr := b.run(stubApplets,
		"STUB_REAL_TRUNCATE="+realTruncate,
		"STUB_TOOL_LOG="+logPath,
		"STUB_FAILURE_TOOL="+failTool,
		"STUB_FAILURE_STATUS="+strconv.Itoa(failureStatus),
	)
	log, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		b.t.Fatalf("no truncate invocation recorded before expected %s failure\n%s", failTool, out)
	}
	if err != nil {
		b.t.Fatalf("read tool log: %v\n%s", err, out)
	}
	calls := strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	want := rollbackTools[:]
	if failTool == "truncate" {
		want = want[:1]
	}
	var staged string
	for i, tool := range want {
		prefix := tool + "\t"
		if i >= len(calls) || !strings.HasPrefix(calls[i], prefix) {
			b.t.Fatalf("missing expected %s invocation at position %d (tool log: %q)\n%s", tool, i+1, log, out)
		}
		path := strings.TrimPrefix(calls[i], prefix)
		if i == 0 {
			staged = path
			if filepath.Dir(filepath.Dir(staged)) != filepath.Dir(b.out) || filepath.Base(staged) != filepath.Base(b.out) {
				b.t.Fatalf("%s received %q, want a staged image beside %q\n%s", tool, staged, b.out, out)
			}
		} else if path != staged {
			b.t.Fatalf("%s received %q, want the image allocated at %q\n%s", tool, path, staged, out)
		}
	}
	if len(calls) != len(want) {
		b.t.Fatalf("unexpected tool invocations after %s (tool log: %q)\n%s", failTool, log, out)
	}
	requireFailure(b.t, out, runErr)
	if status := runErr.(*exec.ExitError).ExitCode(); status != failureStatus {
		b.t.Fatalf("exit status = %d, want injected %s failure status %d\n%s", status, failTool, failureStatus, out)
	}
}

func TestAllocationOrFormatFailureKeepsAnExistingImage(t *testing.T) {
	requireMkfs(t)
	for _, tool := range rollbackTools {
		t.Run(tool, func(t *testing.T) {
			b := newRootfsBuild(t)
			if out, err := b.runSized("8"); err != nil {
				t.Fatalf("the first build failed: %v\n%s", err, out)
			}
			before, err := os.ReadFile(b.out)
			if err != nil {
				t.Fatal(err)
			}
			b.requireToolFailure(tool)
			after, err := os.ReadFile(b.out)
			if err != nil {
				t.Fatalf("the earlier image is gone: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the earlier image changed")
			}
			b.requireOutputDirEntries("rootfs.ext4")
			b.requireNoScratch()
		})
	}
}

func TestAllocationOrFormatFailureCreatesNoImage(t *testing.T) {
	for _, tool := range rollbackTools {
		t.Run(tool, func(t *testing.T) {
			b := newRootfsBuild(t)
			b.requireToolFailure(tool)
			if _, statErr := os.Stat(b.out); !os.IsNotExist(statErr) {
				t.Fatalf("the failed build left an output file: %v", statErr)
			}
			b.requireOutputDirEntries()
			b.requireNoScratch()
		})
	}
}

func TestSuccessfulBuildReplacesAnEarlierImage(t *testing.T) {
	requireMkfs(t)
	b := newRootfsBuild(t)
	if err := os.WriteFile(b.out, []byte("an earlier image"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := b.runSized("12")
	if err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}
	info, statErr := os.Stat(b.out)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if want := int64(12 << 20); info.Size() != want {
		t.Fatalf("image size = %d, want %d", info.Size(), want)
	}
	// The ext4 superblock magic 0xEF53 sits at byte offset 1080.
	data, err := os.ReadFile(b.out)
	if err != nil {
		t.Fatal(err)
	}
	if data[1080] != 0x53 || data[1081] != 0xEF {
		t.Fatalf("no ext4 superblock magic at offset 1080: % x", data[1080:1082])
	}
	if info.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("image mode = %v, want owner read and write", info.Mode().Perm())
	}
	if want := "wrote " + b.out + "\n"; !bytes.HasSuffix([]byte(out), []byte(want)) {
		t.Fatalf("output = %q, want suffix %q", out, want)
	}
	b.requireOutputDirEntries("rootfs.ext4")
	b.requireNoScratch()
}

func TestDirectoryAtTheOutputPathFailsTheBuild(t *testing.T) {
	b := newRootfsBuild(t)
	if err := os.Mkdir(b.out, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := b.runSized(strconv.Itoa(8))
	requireFailure(t, out, err)
	entries, readErr := os.ReadDir(b.out)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the build wrote %d entries into the directory", len(entries))
	}
}

func TestDirectoryCreatedDuringFormattingFailsTheBuild(t *testing.T) {
	b := newRootfsBuild(t)
	const mkfs = `#!/usr/bin/env bash
set -eu
mkdir -- "$STUB_OUTPUT"
printf 'keep this file\n' > "$STUB_OUTPUT/sentinel"
`
	if err := os.WriteFile(filepath.Join(b.dir, "bin", "mkfs.ext4"), []byte(mkfs), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := b.run(stubApplets, "STUB_OUTPUT="+b.out)
	requireFailure(t, out, err)
	if !strings.Contains(out, b.out) || strings.Contains(out, "wrote ") {
		t.Fatalf("want a diagnostic naming %s and no success message:\n%s", b.out, out)
	}
	entries, err := os.ReadDir(b.out)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("want only the sentinel in %s, got %v (read error: %v)", b.out, entries, err)
	}
	data, err := os.ReadFile(filepath.Join(b.out, "sentinel"))
	if err != nil || string(data) != "keep this file\n" {
		t.Fatalf("sentinel = %q (read error: %v)", data, err)
	}
	b.requireOutputDirEntries("rootfs.ext4")
	b.requireNoScratch()
}
