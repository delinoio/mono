// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func newInterruptedWrite(t *testing.T, h *harness) *ownsandv1.WriteFileRequest {
	t.Helper()
	target := filepath.Join(t.TempDir(), "target")
	releaseOnCleanup(t, h)
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("new"),
	}
}

// interruptBeforeFirstChunk stops creation after the intent has the file identity
// and before apply saves the time of the first accepted chunk.
func interruptBeforeFirstChunk(t *testing.T, h *harness, req *ownsandv1.WriteFileRequest, expose bool) writeRecord {
	t.Helper()
	original := stageLink
	defer func() { stageLink = original }()
	stop := errors.New("simulated helper exit")
	var intent writeRecord
	stageLink = func(seed, staging string) error {
		var err error
		intent, err = readWriteRecord(h.srv.writes.recordPath(req.GetWriteId()))
		if err != nil {
			t.Fatal(err)
		}
		if intent.Identity == nil || intent.Reservation.file() != seed || intent.TmpPath != staging {
			t.Fatal("creation intent does not describe the reserved file")
		}
		if expose {
			if err := os.Link(seed, staging); err != nil {
				t.Fatal(err)
			}
		}
		panic(stop)
	}
	func() {
		defer func() {
			if got := recover(); got != stop {
				t.Fatalf("expected to interrupt stage creation, got %v", got)
			}
		}()
		if _, err := h.srv.writes.entry(req); err != nil {
			t.Fatal(err)
		}
	}()
	return intent
}

func TestWriteRecoveryCreationIntent(t *testing.T) { //nolint:paralleltest // Replaces stageLink while interrupting initialization.
	for _, state := range []string{"intent only", "partial refresh", "no staging", "pre-existing", "replacement", "missing anchor"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t, nil)
			req := newInterruptedWrite(t, h)
			intent := interruptBeforeFirstChunk(t, h, req, state != "no staging" && state != "pre-existing")
			stage, reserve := intent.TmpPath, intent.Reservation.Dir
			record := h.srv.writes.recordPath(req.GetWriteId())
			switch state {
			case "partial refresh":
				if err := os.WriteFile(record+".tmp", []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "replacement", "pre-existing":
				if state == "replacement" {
					if err := os.Remove(stage); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(stage, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing anchor":
				if err := os.Remove(intent.Reservation.file()); err != nil {
					t.Fatal(err)
				}
			}

			h = restart(t, h, nil)
			if state == "no staging" {
				if exists(reserve) || exists(record) {
					t.Fatal("startup kept the intent or reservation with no staging file")
				}
			} else {
				assertWriteLostAfterRestart(t, h, req)
				h.clock.Advance(14 * time.Minute)
				h.srv.Sweep()
				if !exists(stage) || !exists(record) {
					t.Fatal("sweep removed the staging file or intent before expiry")
				}
				h.clock.Advance(2 * time.Minute)
				h.srv.Sweep()
				switch state {
				case "pre-existing", "replacement":
					if got, err := os.ReadFile(stage); err != nil || string(got) != "foreign" {
						t.Fatalf("foreign file = %q, %v", got, err)
					}
				case "missing anchor":
					if !exists(stage) {
						t.Fatal("sweep removed a staging file without its ownership proof")
					}
				default:
					if exists(stage) {
						t.Fatal("sweep kept the owned staging file after expiry")
					}
				}
				if exists(reserve) || len(recordFiles(t, h)) != 0 {
					t.Fatal("sweep kept the reservation or record")
				}
			}
			if got, err := os.ReadFile(req.GetPath()); err != nil || string(got) != "old" {
				t.Fatalf("destination = %q, %v", got, err)
			}
		})
	}
}

func TestWriteRecoveryRetiresUnpublishedReservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	req := newInterruptedWrite(t, h)
	parent := filepath.Dir(req.GetPath())
	reserve := filepath.Join(parent, reservationPrefix+"fixture")
	if err := mkdirWriteReservation(reserve); err != nil {
		t.Fatal(err)
	}
	identity, err := identityOfPath(reserve)
	if err != nil {
		t.Fatal(err)
	}
	entry := &writeEntry{
		id: req.GetWriteId(), tmp: tempPath(req.GetPath(), req.GetWriteId()), lastChunk: h.clock.Now(),
		recordPath:  h.srv.writes.recordPath(req.GetWriteId()),
		reservation: &writeReservation{Dir: reserve, Identity: identity},
	}
	if err := saveWriteRecord(entry, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.reservation.file(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry.tmp, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	h = restart(t, h, nil)
	if exists(reserve) || len(recordFiles(t, h)) != 0 {
		t.Fatal("startup kept an unpublished reservation")
	}
	if got, err := os.ReadFile(entry.tmp); err != nil || string(got) != "foreign" {
		t.Fatalf("foreign file = %q, %v", got, err)
	}
}

func TestWriteRecoveryLegacyRecordVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []int{writeRecordVersionUnixNano, writeRecordVersionSplitTime} {
		h := newHarness(t, nil)
		req := newInterruptedWrite(t, h)
		stage := tempPath(req.GetPath(), req.GetWriteId())
		if err := os.WriteFile(stage, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := identityOfPath(stage)
		if err != nil {
			t.Fatal(err)
		}
		rec := writeRecord{Version: version, WriteID: req.GetWriteId(), TmpPath: stage, Identity: &id}
		now := h.clock.Now()
		if version == writeRecordVersionUnixNano {
			nano := now.UnixNano()
			rec.LastNano = &nano
		} else {
			seconds, nano := now.Unix(), now.Nanosecond()
			rec.LastSeconds, rec.LastNanosecond = &seconds, &nano
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.srv.writes.recordPath(req.GetWriteId()), data, 0o600); err != nil {
			t.Fatal(err)
		}
		h = restart(t, h, nil)
		assertWriteLostAfterRestart(t, h, req)
		h.clock.Advance(OperationRetention)
		h.srv.Sweep()
		if exists(stage) || len(recordFiles(t, h)) != 0 {
			t.Fatalf("version %d staging file or record remains after expiry", version)
		}
	}
}

func TestWriteRecoveryFailedRefreshRetainsIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, req := startPartialWrite(t, h)
	record := h.srv.writes.recordPath(req.GetWriteId())
	intent, err := readWriteRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(record+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	chunk := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: req.GetWriteId(), Offset: 7, Data: []byte("more")}
	if _, errd := h.write(chunk); errd != nil {
		t.Fatal(errd)
	}
	after, err := readWriteRecord(record)
	if err != nil || !after.lastChunk.Equal(intent.lastChunk) {
		t.Fatalf("failed refresh changed the durable intent: %v", err)
	}
	h = restart(t, h, nil)
	assertWriteLostAfterRestart(t, h, req)
	h.clock.Advance(OperationRetention)
	h.srv.Sweep()
	if exists(intent.TmpPath) || exists(intent.Reservation.Dir) || exists(record) {
		t.Fatal("sweep did not clean the write after a failed refresh")
	}
}

func TestWriteRecoveryCreationIntentRetriesStagingCleanup(t *testing.T) { //nolint:paralleltest // Replaces stageLink while interrupting initialization.
	h := newHarness(t, nil)
	req := newInterruptedWrite(t, h)
	intent := interruptBeforeFirstChunk(t, h, req, true)
	h = restart(t, h, nil)
	h.srv.writes.removeFile = func(path string) error {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrPermission}
	}
	h.clock.Advance(OperationRetention)
	h.srv.Sweep()
	if !exists(intent.TmpPath) || !exists(intent.Reservation.Dir) || len(recordFiles(t, h)) != 1 {
		t.Fatal("failed cleanup did not retain the staging file and proof")
	}
	if _, errd := h.write(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND ||
		!strings.Contains(errd.GetMessage(), "expired") {
		t.Fatalf("write while cleanup is pending: %v", errd)
	}
	h.srv.writes.removeFile = os.Remove
	h.srv.Sweep()
	if exists(intent.TmpPath) || exists(intent.Reservation.Dir) || len(recordFiles(t, h)) != 0 {
		t.Fatal("cleanup retry did not remove staging and recovery records")
	}
}
