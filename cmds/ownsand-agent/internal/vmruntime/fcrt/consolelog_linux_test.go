// SPDX-License-Identifier: Apache-2.0

package fcrt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logRecords collects the JSON records of a test logger.
type logRecords struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logRecords) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func newTestLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

func (l *logRecords) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCappedLogKeepsPrefixAndConsumesRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFile)
	rec := &logRecords{}
	w, err := openCappedLog(path, 64, newTestLogger(rec), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	var sent int
	for i := 0; i < 10; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, 20)
		n, err := w.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("write %d = %d, %v: the writer must consume every byte", i, n, err)
		}
		sent += n
		want = append(want, chunk...)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[:64]) {
		t.Fatalf("file = %q, want the first 64 bytes %q (sent %d)", got, want[:64], sent)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	var capEvents int
	for _, r := range rec.records(t) {
		if r["sandbox_id"] != "sbx" {
			t.Fatalf("record without the sandbox ID: %v", r)
		}
		if strings.Contains(r["msg"].(string), "reached the cap") {
			capEvents++
		}
		if strings.Contains(string(mustJSON(t, r)), "aaaa") {
			t.Fatalf("console content in a log record: %v", r)
		}
	}
	if capEvents != 1 {
		t.Fatalf("%d cap events logged, want exactly 1", capEvents)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCappedLogBelowCapIsCaptured(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFile)
	rec := &logRecords{}
	w, err := openCappedLog(path, 64, newTestLogger(rec), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("boot ok\n"))
	w.Write([]byte("ready\n"))
	w.Close()
	got, _ := os.ReadFile(path)
	if string(got) != "boot ok\nready\n" {
		t.Fatalf("file = %q", got)
	}
	if n := len(rec.records(t)); n != 0 {
		t.Fatalf("%d log records below the cap", n)
	}
}

// An oversized log (written by a version without a cap) is trimmed on open,
// and a later launch in the same directory counts what is already there.
func TestCappedLogTrimsAndCountsExistingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFile)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &logRecords{}
	w, err := openCappedLog(path, 64, newTestLogger(rec), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() != 64 {
		t.Fatalf("size after open = %d, want 64", st.Size())
	}
	w.Write([]byte("more"))
	w.Close()
	if st, _ := os.Stat(path); st.Size() != 64 {
		t.Fatalf("size after a write at the cap = %d, want 64", st.Size())
	}

	// First launch writes 40 bytes, a second launch (boot fallback) may add 24.
	path2 := filepath.Join(t.TempDir(), logFile)
	w1, _ := openCappedLog(path2, 64, quiet(), "sbx")
	w1.Write(bytes.Repeat([]byte("1"), 40))
	w1.Close()
	w2, err := openCappedLog(path2, 64, quiet(), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	w2.Write(bytes.Repeat([]byte("2"), 100))
	w2.Close()
	got, _ := os.ReadFile(path2)
	want := strings.Repeat("1", 40) + strings.Repeat("2", 24)
	if string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

// Stdout and stderr of one process write through the same value
// concurrently; run with -race.
func TestCappedLogConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFile)
	w, err := openCappedLog(path, 1000, quiet(), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if n, err := w.Write([]byte("0123456789")); n != 10 || err != nil {
					t.Errorf("write = %d, %v", n, err)
				}
			}
		}()
	}
	wg.Wait()
	w.Close()
	if st, _ := os.Stat(path); st.Size() != 1000 {
		t.Fatalf("size = %d, want 1000", st.Size())
	}
}

// A failing file write is logged once with the sandbox ID and the error; the
// writer keeps consuming, so the child never blocks.
func TestCappedLogWriteFailureIsLoggedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFile)
	rec := &logRecords{}
	w, err := openCappedLog(path, 64, newTestLogger(rec), "sbx")
	if err != nil {
		t.Fatal(err)
	}
	w.f.Close() // every later file write fails
	for i := 0; i < 3; i++ {
		if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	var failures int
	for _, r := range rec.records(t) {
		if strings.Contains(r["msg"].(string), "write failed") {
			failures++
			if r["sandbox_id"] != "sbx" || r["error"] == nil || r["error"] == "" {
				t.Fatalf("failure record without sandbox_id and error: %v", r)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("%d failure records, want 1", failures)
	}
}

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
	r := &Runtime{opts: Options{Binary: bin, VMsDir: vmsDir}, logger: quiet(), vms: map[string]*vm{}}
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
	if int64(len(got)) != consoleLogLimit {
		t.Fatalf("log size = %d, want the cap %d", len(got), consoleLogLimit)
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
	if st, _ := os.Stat(path); st.Size() != consoleLogLimit {
		t.Fatalf("log size after a second launch = %d, want %d", st.Size(), consoleLogLimit)
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
	r := &Runtime{opts: Options{Binary: bin}, logger: quiet(), vms: map[string]*vm{}}
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	if err := os.WriteFile(path, bytes.Repeat([]byte("o"), int(consoleLogLimit)+4096), 0o600); err != nil {
		t.Fatal(err)
	}
	runFake(t, r, &vm{id: "sbx", dir: dir})
	if st, _ := os.Stat(path); st.Size() != consoleLogLimit {
		t.Fatalf("log size = %d, want %d", st.Size(), consoleLogLimit)
	}
}

func TestStartProcessKeepsShortOutput(t *testing.T) {
	bin := writeFakeFirecracker(t, "echo out; echo err >&2; exit 0")
	r := &Runtime{opts: Options{Binary: bin}, logger: quiet(), vms: map[string]*vm{}}
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
