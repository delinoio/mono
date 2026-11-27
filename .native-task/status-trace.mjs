import fs from "node:fs";
import path from "node:path";

const platform = process.argv[2];
const fixed = platform === "windows-fixed";
if (platform !== "windows" && platform !== "linux" && !fixed) {
  throw new Error("Usage: node status-trace.mjs windows|windows-fixed|linux");
}
const store = path.resolve("cmds", "ownsand-agent", "internal", "store");
const scratch = path.resolve(".t3-tmp", "status-trace-" + platform);
fs.mkdirSync(scratch, { recursive: true });
const replace = {};

function overlay(file, from, to) {
  const original = path.join(store, file);
  const content = fs.readFileSync(original, "utf8");
  if (content.split(from).length !== 2) {
    throw new Error("Expected exactly one trace insertion in " + file);
  }
  writeOverlay(file, content.replace(from, to));
}

function writeOverlay(file, content) {
  const generated = path.join(scratch, file);
  fs.writeFileSync(generated, content);
  replace[path.join(store, file)] = generated;
}

if (fixed) {
  const source = path.join(store, "status_windows.go");
  const read = "readWindowsStatus(path, os.ReadFile)";
  const move =
    "windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)";
  let content = fs.readFileSync(source, "utf8");
  if (content.split(read).length !== 2 || content.split(move).length !== 2) {
    throw new Error("Expected exactly one read and one move trace insertion");
  }
  content = content.replace(read, "readWindowsStatus(path, traceStatusRead)");
  content = content.replace(
    move,
    'traceStatusMove("MoveFileEx", func() error { return ' + move + " })",
  );
  writeOverlay("status_windows.go", content);
} else if (platform === "windows") {
  overlay(
    "store.go",
    "data, err := os.ReadFile(path)",
    "data, err := traceStatusRead(path)",
  );
  overlay(
    "rename_windows.go",
    "os.Rename(oldPath, newPath)",
    "traceStatusRename(os.Rename, oldPath, newPath)",
  );
} else {
  const source = path.join(store, "status_other.go");
  const content = fs.readFileSync(source, "utf8");
  writeOverlay(
    "status_other.go",
    content
      .replace('"os"', "")
      .replace("return os.ReadFile(path)", "return traceStatusRead(path)"),
  );
  overlay(
    "rename_other.go",
    "os.Rename(oldPath, newPath)",
    "traceStatusRename(os.Rename, oldPath, newPath)",
  );
}

writeOverlay(
  "status_trace.go",
  `// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var statusTrace struct {
	started, finished, reads atomic.Uint64
	mu sync.Mutex
	samples []string
	counts map[string]int
}

const statusTraceStrict = ${fixed}

func traceStatusError(call string, err error, began time.Time, started, finished uint64) {
	var errNo syscall.Errno
	var pathErr *os.PathError
	var linkErr *os.LinkError
	op := ""
	raw := "unknown"
	if errors.As(err, &errNo) {
		raw = fmt.Sprint(uintptr(errNo))
	}
	if errors.As(err, &pathErr) {
		op = pathErr.Op
	}
	if errors.As(err, &linkErr) {
		op = linkErr.Op
	}
	ended := time.Now()
	key := fmt.Sprintf("%s op=%s errno=%s not_exist=%t", call, op, raw, errors.Is(err, os.ErrNotExist))
	message := fmt.Sprintf("%s begin_ns=%d end_ns=%d rename_started_before=%d rename_finished_before=%d rename_started_after=%d rename_finished_after=%d",
		key, began.UnixNano(), ended.UnixNano(), started, finished, statusTrace.started.Load(), statusTrace.finished.Load())
	if os.Getenv("OWNSAND_STATUS_TRACE_STDERR") == "1" {
		fmt.Fprintln(os.Stderr, "status-trace "+message)
	}
	statusTrace.mu.Lock()
	defer statusTrace.mu.Unlock()
	if statusTrace.counts == nil {
		statusTrace.counts = make(map[string]int)
	}
	if statusTrace.counts[key] < 3 {
		statusTrace.samples = append(statusTrace.samples, message)
	}
	statusTrace.counts[key]++
}

func traceStatusRead(path string) ([]byte, error) {
	if filepath.Base(path) != StatusFile {
		return os.ReadFile(path)
	}
	began := time.Now()
	started, finished := statusTrace.started.Load(), statusTrace.finished.Load()
	data, err := os.ReadFile(path)
	statusTrace.reads.Add(1)
	if err != nil {
		traceStatusError("os.ReadFile", err, began, started, finished)
	}
	return data, err
}

func traceStatusRename(rename func(string, string) error, oldPath, newPath string) error {
	if filepath.Base(newPath) != StatusFile {
		return rename(oldPath, newPath)
	}
	return traceStatusMove("os.Rename", func() error { return rename(oldPath, newPath) })
}

func traceStatusMove(operation string, move func() error) error {
	began := time.Now()
	finished := statusTrace.finished.Load()
	started := statusTrace.started.Add(1)
	err := move()
	statusTrace.finished.Store(started)
	if err != nil {
		traceStatusError(operation, err, began, started, finished)
	}
	return err
}
`,
);

writeOverlay(
  "status_trace_test.go",
  String.raw`// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type traceReport struct {
	Seq int
	Padding string
}

func TestTraceStatusReplacement(t *testing.T) {
	dir := t.TempDir()
	// Both fixtures exercise the production reader and writer in a disposable
	// directory. The Windows state-directory DACL is independent of replacement.
	reader := &Store{dir: dir}
	writer := &Store{dir: dir}
	statusTrace.started.Store(0)
	statusTrace.finished.Store(0)
	statusTrace.reads.Store(0)
	statusTrace.samples = nil
	statusTrace.counts = nil
	var report traceReport
	if err := reader.LoadStatus(&report); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent initial report: %v", err)
	}
	for _, sample := range statusTrace.samples {
		t.Logf("initial absence control: %s", sample)
	}
	statusTrace.samples = nil
	statusTrace.counts = nil

	cli := os.Getenv("OWNSAND_STATUS_TRACE_CLI")
	var cliReads atomic.Int64
	checkCLI := func(absent bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10 * time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, "status", "--state-dir", dir, "--no-color")
		cmd.Env = append(os.Environ(), "OWNSAND_STATUS_TRACE_STDERR=1")
		var traceOutput bytes.Buffer
		cmd.Stderr = &traceOutput
		out, err := cmd.Output()
		want := []byte("Connection: disconnected (status of ")
		if absent {
			want = []byte("Connection: unknown (the agent has not run)")
		}
		valid := err == nil && bytes.Contains(out, want)
		if !valid {
			t.Errorf("status CLI absent=%t exit=%v expected_connection_line=%t", absent, err, bytes.Contains(out, want))
		}
		if absent || !valid {
			for _, line := range strings.Split(traceOutput.String(), "\n") {
				if strings.HasPrefix(line, "status-trace ") {
					t.Logf("status CLI absent=%t %s", absent, line)
				}
			}
		}
		cliReads.Add(1)
	}
	if cli != "" {
		for _, name := range []string{SandboxesDir, SnapshotsDir} {
			if err := os.Mkdir(reader.Path(name), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.SaveIdentity(&Identity{DeviceID: "fixture-device", OrganizationID: "fixture-org"}); err != nil {
			t.Fatal(err)
		}
		checkCLI(true)
	}
	padding := strings.Repeat("x", 4096)
	if err := writer.SaveStatus(traceReport{Seq: 0, Padding: padding}); err != nil {
		t.Fatalf("first publication failed: %v", err)
	}

	const writes = 300
	readers := 1
	if raw := os.Getenv("OWNSAND_STATUS_TRACE_READERS"); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count < 1 || count > 8 {
			t.Fatal("OWNSAND_STATUS_TRACE_READERS must be from 1 through 8")
		}
		readers = count
	}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		close(stop)
		workers.Wait()
		t.Logf("read_calls=%d cli_calls=%d rename_attempts=%d rename_completions=%d",
			statusTrace.reads.Load(), cliReads.Load(), statusTrace.started.Load(), statusTrace.finished.Load())
		for _, sample := range statusTrace.samples {
			t.Log(sample)
		}
		t.Logf("error_counts=%v", statusTrace.counts)
	}()
	ready := make(chan struct{}, readers+1)
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			last := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				var report traceReport
				err := reader.LoadStatus(&report)
				// The original fixture permits 5 (access denied) and 32 (sharing
				// violation). The trace retains both codes.
				if !statusTraceStrict && runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32))) {
					continue
				}
				if err != nil || report.Padding != padding || report.Seq < last || report.Seq > writes {
					t.Errorf("invalid report after first publication: err=%v bytes=%d seq=%d previous=%d", err, len(report.Padding), report.Seq, last)
					return
				}
				last = report.Seq
			}
		}()
	}
	if cli != "" {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					checkCLI(false)
				}
			}
		}()
		<-ready
	}
	for range readers {
		<-ready
	}
	for seq := 1; seq <= writes; seq++ {
		if err := writer.SaveStatus(traceReport{Seq: seq, Padding: padding}); err != nil {
			t.Fatalf("publication %d failed: %v", seq, err)
		}
	}
	if err := reader.LoadStatus(&report); err != nil || report.Seq != writes || report.Padding != padding {
		t.Fatalf("last report: error=%v seq=%d padding_bytes=%d", err, report.Seq, len(report.Padding))
	}
	if cli != "" {
		checkCLI(false)
	}
}
`,
);

const overlayPath = path.join(scratch, "overlay.json");
fs.writeFileSync(overlayPath, JSON.stringify({ Replace: replace }));
console.log(overlayPath);
