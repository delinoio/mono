// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// writeFakeFirecracker writes an executable shell script that stands in for
// the firecracker binary and runs body.
func writeFakeFirecracker(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// runFake starts the fake binary through startProcess and waits for it to
// exit. The fake never opens the API socket, so startProcess itself reports
// "exited during start"; the log file is what is under test.
func runFake(t *testing.T, r *Runtime, v *vm) {
	t.Helper()
	_ = r.startProcess(context.Background(), v)
	select {
	case <-v.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("fake firecracker did not exit")
	}
}

func TestStartProcessCapsFirecrackerLog(t *testing.T) {
	// Marker line first, then 2 MB on stdout and 2 MB on stderr: more than
	// the cap by far, and a child that would die or block on a failed write.
	bin := writeFakeFirecracker(t, `echo START
head -c 2000000 /dev/zero | tr '\0' a
head -c 2000000 /dev/zero | tr '\0' b >&2
echo UNREACHABLE-TAIL
exit 0`)
	vmsDir := t.TempDir()
	r := bareRuntime(Options{Binary: bin, VMsDir: vmsDir})
	dir := r.vmDir("sbx")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := &vm{id: "sbx", dir: dir}
	runFake(t, r, v)

	path := filepath.Join(dir, logFile)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != vmruntime.ConsoleLogLimit {
		t.Fatalf("log size = %d, want the cap %d", len(got), vmruntime.ConsoleLogLimit)
	}
	if !bytes.HasPrefix(got, []byte("START\naaaa")) {
		t.Fatalf("log does not keep the first bytes: %q", got[:16])
	}
	if bytes.Contains(got, []byte("UNREACHABLE-TAIL")) {
		t.Fatal("output beyond the cap was kept")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}

	// A second launch in the same directory (boot fallback) cannot grow the
	// log beyond the cap.
	v2 := &vm{id: "sbx", dir: dir}
	runFake(t, r, v2)
	if st, _ := os.Stat(path); st.Size() != vmruntime.ConsoleLogLimit {
		t.Fatalf("log size after a second launch = %d, want %d", st.Size(), vmruntime.ConsoleLogLimit)
	}

	// The log lives and dies with the VM directory.
	if err := r.Delete(context.Background(), "sbx"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("log kept after Delete: %v", err)
	}
}

func TestStartProcessTrimsOversizedExistingLog(t *testing.T) {
	bin := writeFakeFirecracker(t, "echo late; exit 0")
	r := bareRuntime(Options{Binary: bin})
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	if err := os.WriteFile(path, bytes.Repeat([]byte("o"), int(vmruntime.ConsoleLogLimit)+4096), 0o600); err != nil {
		t.Fatal(err)
	}
	runFake(t, r, &vm{id: "sbx", dir: dir})
	if st, _ := os.Stat(path); st.Size() != vmruntime.ConsoleLogLimit {
		t.Fatalf("log size = %d, want %d", st.Size(), vmruntime.ConsoleLogLimit)
	}
}

func TestStartProcessKeepsShortOutput(t *testing.T) {
	bin := writeFakeFirecracker(t, "echo out; echo err >&2; exit 0")
	r := bareRuntime(Options{Binary: bin})
	dir := t.TempDir()
	runFake(t, r, &vm{id: "sbx", dir: dir})
	got, err := os.ReadFile(filepath.Join(dir, logFile))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); s != "out\nerr\n" {
		t.Fatalf("log = %q, want both streams in order", s)
	}
}
