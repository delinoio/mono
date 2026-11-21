// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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
	path := filepath.Join(t.TempDir(), "firecracker.log")
	rec := &logRecords{}
	w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", ConsoleLogKindFirecracker)
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
	// Windows reports 0666 for every writable file; POSIX modes do not apply.
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
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
	path := filepath.Join(t.TempDir(), "firecracker.log")
	rec := &logRecords{}
	w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", ConsoleLogKindFirecracker)
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
	path := filepath.Join(t.TempDir(), "firecracker.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &logRecords{}
	w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", ConsoleLogKindFirecracker)
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
	path2 := filepath.Join(t.TempDir(), "firecracker.log")
	w1, _ := OpenCappedLog(path2, 64, newTestLogger(io.Discard), "sbx", ConsoleLogKindFirecracker)
	w1.Write(bytes.Repeat([]byte("1"), 40))
	w1.Close()
	w2, err := OpenCappedLog(path2, 64, newTestLogger(io.Discard), "sbx", ConsoleLogKindFirecracker)
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
	path := filepath.Join(t.TempDir(), "firecracker.log")
	w, err := OpenCappedLog(path, 1000, newTestLogger(io.Discard), "sbx", ConsoleLogKindFirecracker)
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
	path := filepath.Join(t.TempDir(), "firecracker.log")
	rec := &logRecords{}
	w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", ConsoleLogKindFirecracker)
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

// The event text comes from the kind: the Firecracker wording is stable, and
// the Hyper-V console uses its own noun.
func TestCappedLogEventTextFollowsKind(t *testing.T) {
	cases := []struct {
		kind ConsoleLogKind
		noun string
	}{
		{ConsoleLogKindFirecracker, "firecracker log"},
		{ConsoleLogKindConsole, "console log"},
	}
	for _, tc := range cases {
		t.Run(tc.noun, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.log")
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := &logRecords{}
			w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte("more"))
			w.f.Close()
			w.Write([]byte("more")) // the cap is already reached: no failure event
			w.Close()
			var msgs []string
			for _, r := range rec.records(t) {
				msgs = append(msgs, r["msg"].(string))
			}
			want := []string{
				"oversized " + tc.noun + " trimmed to the cap",
				tc.noun + " reached the cap; discarding further output",
			}
			if strings.Join(msgs, "|") != strings.Join(want, "|") {
				t.Fatalf("messages = %q, want %q", msgs, want)
			}
		})
	}
}

func TestOpenCappedLogRejectsUnspecifiedKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if _, err := OpenCappedLog(path, 64, newTestLogger(io.Discard), "sbx", ConsoleLogKindUnspecified); err == nil {
		t.Fatal("unspecified kind accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created for a rejected kind: %v", err)
	}
}

// A write after Close (a producer that outlives the copy) is consumed
// without an error and without a failure event.
func TestCappedLogWriteAfterCloseIsConsumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	rec := &logRecords{}
	w, err := OpenCappedLog(path, 64, newTestLogger(rec), "sbx", ConsoleLogKindConsole)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("ab"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if n, err := w.Write([]byte("cdef")); n != 4 || err != nil {
		t.Fatalf("write after Close = %d, %v", n, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "ab" {
		t.Fatalf("file = %q", got)
	}
	if n := len(rec.records(t)); n != 0 {
		t.Fatalf("%d log records for a write after Close", n)
	}
}
