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

func TestWriteRecoveryRecordHandleBlocksPromotionAndCleanup(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	staging := tempPath(target, req.GetWriteId())
	canonical, temp := demoteRecordToTemp(t, h, req)
	path, err := windows.UTF16PtrFromString(temp)
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
