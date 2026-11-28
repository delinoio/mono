// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"golang.org/x/sys/windows"
)

func TestWriteExpiryOfPrefixedLongStagingPath(t *testing.T) {
	t.Parallel()
	testWriteExpiryOfLongStagingPath(t, extendedPath)
}

func TestWriteRecoveryRecordHandleBlocksPromotionAndCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	staging := tempPath(target, req.GetWriteId())
	canonical, temp := demoteRecordToTemp(t, h, req)
	path, err := windows.UTF16PtrFromString(extendedPath(temp))
	if err != nil {
		t.Fatal(err)
	}

	var h2 *harness
	func() {
		// Allow the helper to read the record, but deny rename and removal
		// until this handle closes.
		handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}()
		if err := os.Rename(temp, canonical); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) &&
			!errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("promotion with a handle open without delete sharing: %v", err)
		}
		h2 = restart(t, h, nil)
		if got := recordFiles(t, h2); len(got) != 1 || got[0] != filepath.Base(temp) {
			t.Fatalf("records after failed promotion: %v", got)
		}
		_, errd := h2.write(req)
		if errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND ||
			errd.GetMessage() != "write was lost in a helper restart" {
			t.Fatalf("recovered write response: %v", errd)
		}

		h2.clock.Advance(15 * time.Minute)
		h2.srv.Sweep()
		if exists(staging) || !exists(temp) {
			t.Fatal("expiry should remove the staging file and retain the locked record")
		}
	}()

	h2.srv.Sweep()
	if got := recordFiles(t, h2); len(got) != 0 {
		t.Fatalf("records after the handle closed: %v", got)
	}
}

func TestWriteRecoveryCreationIntentWithHeldStagingHandle(t *testing.T) { //nolint:paralleltest // Replaces stageLink while interrupting initialization.
	h := newHarness(t, nil)
	req := newInterruptedWrite(t, h)
	intent := interruptBeforeFirstChunk(t, h, req, true)
	path, err := windows.UTF16PtrFromString(extendedPath(intent.TmpPath))
	if err != nil {
		t.Fatal(err)
	}
	h = restart(t, h, nil)
	assertWriteLostAfterRestart(t, h, req)
	func() {
		handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
			windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}()
		h.clock.Advance(OperationRetention)
		h.srv.Sweep()
		if !exists(intent.TmpPath) || !exists(intent.Reservation.file()) || len(recordFiles(t, h)) != 1 {
			t.Fatal("a handle without delete sharing must leave the staging file and its proof in place")
		}
	}()
	h.srv.Sweep()
	if exists(intent.TmpPath) || exists(intent.Reservation.Dir) || len(recordFiles(t, h)) != 0 {
		t.Fatal("cleanup retry did not remove staging and its proof after the handle closed")
	}
	if got, err := os.ReadFile(req.GetPath()); err != nil || string(got) != "old" {
		t.Fatalf("destination = %q, %v", got, err)
	}
}

func TestWriteFileInheritsDestinationDACL(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	req := newInterruptedWrite(t, h)
	sibling := filepath.Join(filepath.Dir(req.GetPath()), "sibling")
	if err := os.WriteFile(sibling, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	req.Finish = &ownsandv1.WriteFileFinish{SizeBytes: int64(len(req.GetData()))}
	if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
		t.Fatalf("finish = %v, %v", resp, errd)
	}
	getDACL := func(path string) string {
		t.Helper()
		descriptor, err := windows.GetNamedSecurityInfo(extendedPath(path), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		return descriptor.String()
	}
	if got, want := getDACL(req.GetPath()), getDACL(sibling); got != want {
		t.Fatalf("published DACL = %q, want sibling DACL %q", got, want)
	}
}
