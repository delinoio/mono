// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

func accepted(id string) *sandcrewv1.BridgeEvent {
	return &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_InputAccepted{InputAccepted: &sandcrewv1.InputAccepted{InputId: id}}}
}

func TestSequenceAndOffsetsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stdout bytes.Buffer
	log, err := Open(path, Options{Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	// Enough events that the offset gains digits.
	for i := range 120 {
		if err := log.Append(accepted(strings.Repeat("x", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, Options{Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.LastSequence() != 120 || !reopened.HasInput(strings.Repeat("x", 7)) || reopened.HasInput("other") {
		t.Fatalf("recovered sequence %d", reopened.LastSequence())
	}
	if err := reopened.Append(accepted("after")); err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, stdout.Bytes()) {
		t.Fatal("stdout and the log differ")
	}
	offset := 0
	for i, line := range strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n") {
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		offset += len(line)
		event, err := Decode([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if event.GetSequence() != uint64(i+1) || event.GetLogEndOffset() != uint64(offset) {
			t.Fatalf("line %d: sequence %d offset %d, want %d %d", i, event.GetSequence(), event.GetLogEndOffset(), i+1, offset)
		}
	}
}

func TestPartialLastLineIsTerminated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(accepted("a")); err != nil {
		t.Fatal(err)
	}
	log.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"sequence":"2","inpu`)
	f.Close()

	log, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(accepted("b")); err != nil {
		t.Fatal(err)
	}
	log.Close()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || lines[1] != `{"sequence":"2","inpu` {
		t.Fatalf("log lines %q", lines)
	}
	last, err := Decode([]byte(lines[2]))
	if err != nil || last.GetSequence() != 2 || last.GetLogEndOffset() != uint64(len(data)) {
		t.Fatalf("last event %v %v, log size %d", last, err, len(data))
	}
}

// flakyFile writes at most limit bytes per call and fails the first
// failures calls.
type flakyFile struct {
	buf      bytes.Buffer
	failures int
	limit    int
	syncs    int
}

func (f *flakyFile) Write(p []byte) (int, error) {
	if f.failures > 0 {
		f.failures--
		n := min(len(p), f.limit)
		f.buf.Write(p[:n])
		return n, errors.New("transient")
	}
	return f.buf.Write(p)
}
func (f *flakyFile) Sync() error  { f.syncs++; return nil }
func (f *flakyFile) Close() error { return nil }

func TestRetriedAppendContinuesAPartialWrite(t *testing.T) {
	file := &flakyFile{failures: 2, limit: 5}
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"), Options{
		OpenFile:   func(string) (File, error) { return file, nil },
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(accepted("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(file.buf.Bytes()); err != nil || bytes.Count(file.buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("log content %q", file.buf.String())
	}
}

func TestFailedAppendWritesTheFailureToStdout(t *testing.T) {
	file := &flakyFile{failures: 100, limit: 0}
	var stdout bytes.Buffer
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"), Options{
		Stdout:     &stdout,
		OpenFile:   func(string) (File, error) { return file, nil },
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(accepted("a")); !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("error %v", err)
	}
	if file.failures != 100-1-appendRetries {
		t.Fatalf("%d attempts, want %d", 100-file.failures, 1+appendRetries)
	}
	event, err := Decode(stdout.Bytes())
	if err != nil || event.GetBridgeError().GetKind() != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_EVENT_LOG_WRITE_FAILED || event.GetSequence() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
	if err := log.Append(accepted("b")); !errors.Is(err, ErrWriteFailed) {
		t.Fatal("the log accepted an event after a failure")
	}
}

func TestTurnEndedIsSynced(t *testing.T) {
	file := &flakyFile{}
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"), Options{OpenFile: func(string) (File, error) { return file, nil }})
	if err != nil {
		t.Fatal(err)
	}
	log.Append(accepted("a"))
	if file.syncs != 0 {
		t.Fatal("synced before turn_ended")
	}
	log.Append(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_TurnEnded{TurnEnded: &sandcrewv1.TurnEnded{}}})
	if file.syncs != 1 {
		t.Fatalf("syncs %d after turn_ended", file.syncs)
	}
}

func TestStdoutOnlyEventsRepeatTheLastSequence(t *testing.T) {
	file := &flakyFile{}
	var stdout bytes.Buffer
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"), Options{Stdout: &stdout, OpenFile: func(string) (File, error) { return file, nil }})
	if err != nil {
		t.Fatal(err)
	}
	log.Append(accepted("a"))
	logged := file.buf.Len()
	stdout.Reset()
	if err := log.WriteStdoutOnly(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_CredentialUpdate{CredentialUpdate: &sandcrewv1.CredentialUpdate{Revision: 1}}}); err != nil {
		t.Fatal(err)
	}
	event, err := Decode(stdout.Bytes())
	if err != nil || event.GetSequence() != 1 || event.GetLogEndOffset() != uint64(logged) || file.buf.Len() != logged {
		t.Fatalf("stdout-only event %v %v", event, err)
	}
}
