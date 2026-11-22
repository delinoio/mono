// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestFsErrorMapsWindowsDiskFull checks that the Win32 disk full errors map to
// RESOURCE_EXHAUSTED, also when wrapped the way os functions wrap them.
func TestFsErrorMapsWindowsDiskFull(t *testing.T) {
	for _, errno := range []syscall.Errno{windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL} {
		err := &os.PathError{Op: "write", Path: `C:\secret`, Err: errno}
		got := fsError("write", err)
		if got.code != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED || got.msg != "write: no space left on the guest disk" {
			t.Fatalf("errno %d: got %v %q", errno, got.code, got.msg)
		}
	}
}

// TestFsErrorKeepsOtherWindowsErrors checks that an error without a mapping
// stays INTERNAL and does not name the path.
func TestFsErrorKeepsOtherWindowsErrors(t *testing.T) {
	err := &os.PathError{Op: "write", Path: `C:\secret`, Err: windows.ERROR_SHARING_VIOLATION}
	got := fsError("write", err)
	if got.code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL || got.msg != "write: file system error" {
		t.Fatalf("got %v %q", got.code, got.msg)
	}
}

// TestStartFailureMapsWindowsErrors checks the Win32 errors that CreateProcess
// returns for a program that cannot run.
func TestStartFailureMapsWindowsErrors(t *testing.T) {
	cases := []struct {
		name   string
		errno  syscall.Errno
		reason ownsandv1.ExecFailureReason
	}{
		{"bad exe format", windows.ERROR_BAD_EXE_FORMAT, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED},
		{"exe machine type mismatch", windows.ERROR_EXE_MACHINE_TYPE_MISMATCH, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED},
		{"access denied", windows.ERROR_ACCESS_DENIED, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED},
		{"not enough memory", windows.ERROR_NOT_ENOUGH_MEMORY, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_RESOURCE_EXHAUSTED},
		{"out of memory", windows.ERROR_OUTOFMEMORY, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_RESOURCE_EXHAUSTED},
		{"file not found", windows.ERROR_FILE_NOT_FOUND, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND},
		{"other", windows.ERROR_SHARING_VIOLATION, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_OTHER},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("fork/exec: %w", &os.PathError{Op: "fork/exec", Path: `C:\p.exe`, Err: tc.errno})
			if got := startFailure(err).GetFailed().GetReason(); got != tc.reason {
				t.Fatalf("got %v, want %v", got, tc.reason)
			}
		})
	}
}

// TestStartFailureForTextFileNamedExe starts a text file that has the .exe
// extension. CreateProcess rejects it with ERROR_BAD_EXE_FORMAT on older
// Windows versions and with ERROR_EXE_MACHINE_TYPE_MISMATCH on newer ones; the
// exec path reports both as PERMISSION_DENIED.
func TestStartFailureForTextFileNamedExe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-program.exe")
	if err := os.WriteFile(path, []byte("this is not a PE image\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := exec.Command(path).Start()
	if err == nil {
		t.Fatal("starting a text file succeeded")
	}
	if got := startFailure(err).GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED {
		t.Fatalf("start error %v mapped to %v", err, got)
	}
}
