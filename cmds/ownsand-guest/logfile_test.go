// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// smallLogLimit is a rotation limit that the tests exceed with a few bytes.
const smallLogLimit = 8

// writeFile writes data to path and fails the test on an error.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readFile returns the content of path and fails the test on an error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// blockArchive makes the rotation of the log in dir fail by putting a
// non-empty directory at the archive path: the rename cannot replace it on
// any platform. On Windows the same rotation fails when another process
// holds the archive open (see TestOpenServiceLogKeepsArchiveHeldByAnotherHandle).
// It returns the archive path and the path of a file inside it.
func blockArchive(t *testing.T, dir string) (archive, inner string) {
	t.Helper()
	archive = filepath.Join(dir, logFile+".1")
	if err := os.Mkdir(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	inner = filepath.Join(archive, "keep")
	writeFile(t, inner, "archive content")
	return archive, inner
}

// openLogForTest opens the log of dir, closes it when the test ends and
// returns its output.
func openLogForTest(t *testing.T, dir string, limit int64) logOutput {
	t.Helper()
	out, err := openLogFile(dir, limit)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	t.Cleanup(func() { _ = out.Close() })
	return out
}

// logRecords parses the JSON log lines in data.
func logRecords(t *testing.T, data string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestRotateLogThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	if err := rotateLog(path, smallLogLimit); err != nil {
		t.Fatalf("missing log: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("a missing log created an archive")
	}

	writeFile(t, path, "12345678") // exactly the limit
	if err := rotateLog(path, smallLogLimit); err != nil {
		t.Fatalf("log at the limit: %v", err)
	}
	if got := readFile(t, path); got != "12345678" {
		t.Fatalf("log at the limit changed: %q", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("a log at the limit was rotated")
	}

	writeFile(t, path, "123456789") // one byte over the limit
	writeFile(t, path+".1", "older archive")
	if err := rotateLog(path, smallLogLimit); err != nil {
		t.Fatalf("log over the limit: %v", err)
	}
	if got := readFile(t, path+".1"); got != "123456789" {
		t.Fatalf("archive %q, want the rotated log", got)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the rotated log is still at its path")
	}
}

func TestOpenLogFileAtExactlyRotateLimitWarnsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(logRotateBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := openLogForTest(t, dir, logRotateBytes)
	if len(out.Notes) != 0 {
		t.Fatalf("a log of exactly %d bytes produced %d notes", logRotateBytes, len(out.Notes))
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf("a log of exactly %d bytes was rotated", logRotateBytes)
	}
	if _, err := out.W.Write([]byte("appended\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(logRotateBytes + len("appended\n")); st.Size() != want {
		t.Fatalf("log size %d, want %d (append to the existing file)", st.Size(), want)
	}
}

func TestOpenLogFileWarnsOnceWhenRotationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	writeFile(t, path, "old log line\n") // over the limit
	archive, inner := blockArchive(t, dir)

	out := openLogForTest(t, dir, smallLogLimit)
	if len(out.Notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(out.Notes))
	}
	// The log works: the helper writes into the old active file.
	logger := slog.New(slog.NewJSONHandler(out.W, nil))
	for _, note := range out.Notes {
		note(logger)
	}
	logger.Info("later record")

	if got := readFile(t, inner); got != "archive content" {
		t.Fatalf("archive content %q after the failed rotation", got)
	}
	data := readFile(t, path)
	if !strings.HasPrefix(data, "old log line\n") {
		t.Fatalf("the active log lost its content: %q", data)
	}
	records := logRecords(t, data)
	if len(records) != 2 {
		t.Fatalf("got %d records, want the warning and the later record: %q", len(records), data)
	}
	warning := records[0]
	if warning["level"] != "WARN" || warning["operation"] != "rotate" || warning["path"] != path || warning["archive"] != archive {
		t.Fatalf("unexpected warning: %v", warning)
	}
	if msg, _ := warning["msg"].(string); !strings.Contains(msg, "log rotation failed") {
		t.Fatalf("unexpected warning message %q", msg)
	}
	if text, _ := warning["error"].(string); text == "" {
		t.Fatalf("the warning has no error: %v", warning)
	}
}

func TestOpenLogFileRotatesOnTheNextStartAfterTheBlockEnds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	writeFile(t, path, "old log line\n")
	archive, _ := blockArchive(t, dir)

	first := openLogForTest(t, dir, smallLogLimit)
	if len(first.Notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(first.Notes))
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// The block ends.
	if err := os.RemoveAll(archive); err != nil {
		t.Fatal(err)
	}
	second := openLogForTest(t, dir, smallLogLimit)
	if len(second.Notes) != 0 {
		t.Fatalf("got %d notes after the block ended, want none", len(second.Notes))
	}
	if got := readFile(t, archive); got != "old log line\n" {
		t.Fatalf("archive %q, want the rotated log", got)
	}
	if got := readFile(t, path); got != "" {
		t.Fatalf("the new active log %q is not empty", got)
	}
}

func TestOpenLogFileReportsAnUnopenableLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, logFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openLogFile(dir, logRotateBytes); err == nil {
		t.Fatal("openLogFile opened a directory as the log")
	}
}

// TestRunReportsFailedLogRotationInTheActiveLog runs the helper over a log
// that cannot rotate: it starts, serves, and writes one rotation warning
// into the active log.
func TestRunReportsFailedLogRotationInTheActiveLog(t *testing.T) {
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, logFile), "old log line\n")
	_, inner := blockArchive(t, stateDir)
	sock := filepath.Join(shortSocketDir(t), "g.sock")

	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{"--listen", "unix://" + sock, "--state-dir", stateDir, "--clock", "ignore"},
			func(dir string) (logOutput, error) { return openLogFile(dir, smallLogLimit) })
	}()
	t.Cleanup(cancel)
	waitServing(t, sock)
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("helper exit code %d, want 0", c)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not exit")
	}

	data := readFile(t, filepath.Join(stateDir, logFile))
	var warnings int
	for _, record := range logRecords(t, data) {
		if msg, _ := record["msg"].(string); strings.Contains(msg, "log rotation failed") {
			warnings++
			if record["level"] != "WARN" || record["component"] != "ownsand-guest" {
				t.Fatalf("unexpected warning record: %v", record)
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("got %d rotation warnings, want 1; log %q", warnings, data)
	}
	if !bytes.HasPrefix([]byte(data), []byte("old log line\n")) {
		t.Fatalf("the active log lost its content: %q", data)
	}
	if got := readFile(t, inner); got != "archive content" {
		t.Fatalf("archive content %q after the failed rotation", got)
	}
}

// TestRunClosesTheLogWhenItReturns checks that run releases the log output
// on every return after the opener succeeded. A log file handle that stays
// open blocks the removal of the file on Windows.
func TestRunClosesTheLogWhenItReturns(t *testing.T) {
	var closed int
	// An invalid clock mode ends run after the logger exists and before the
	// helper listens.
	code := run(context.Background(), []string{"--state-dir", t.TempDir(), "--clock", "bogus"},
		func(string) (logOutput, error) {
			return logOutput{W: io.Discard, Close: func() error { closed++; return nil }}, nil
		})
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if closed != 1 {
		t.Fatalf("the log was closed %d times, want once", closed)
	}
}
