// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestErrnoMappingOnUnix checks that the POSIX errors keep their mapping.
func TestErrnoMappingOnUnix(t *testing.T) {
	disk := fsError("write", &os.PathError{Op: "write", Path: "/secret", Err: syscall.ENOSPC})
	if disk.code != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED || disk.msg != "write: no space left on the guest disk" {
		t.Fatalf("ENOSPC: %v %q", disk.code, disk.msg)
	}
	cases := []struct {
		errno  syscall.Errno
		reason ownsandv1.ExecFailureReason
	}{
		{syscall.ENOEXEC, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED},
		{syscall.EAGAIN, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_RESOURCE_EXHAUSTED},
		{syscall.ENOMEM, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_RESOURCE_EXHAUSTED},
	}
	for _, tc := range cases {
		err := fmt.Errorf("fork/exec: %w", &os.PathError{Op: "fork/exec", Path: "/p", Err: tc.errno})
		if got := startFailure(err).GetFailed().GetReason(); got != tc.reason {
			t.Fatalf("%v: got %v, want %v", tc.errno, got, tc.reason)
		}
	}
}
