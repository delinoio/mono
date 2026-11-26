// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"testing"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestGuestCommandsAndPathsWorkingDirectory(t *testing.T) {
	windows := sandrunv1.GuestOs_GUEST_OS_WINDOWS
	for _, tc := range []struct {
		os        sandrunv1.GuestOs
		workspace string
		want      string
	}{
		{sandrunv1.GuestOs_GUEST_OS_LINUX, "/home/dev/work", "/"},
		{sandrunv1.GuestOs_GUEST_OS_MACOS, "/Users/dev/work", "/"},
		{windows, `C:\work`, `C:\`},
		{windows, `D:\a\b\`, `D:\`},
		{windows, `c:\work`, `c:\`},
		{windows, `C:work`, ""},
		{windows, `\\host\share\work`, ""},
		{windows, `1:\work`, ""},
		{windows, "", ""},
	} {
		if got := guestRoot(&sandrunv1.DevSession{GuestOs: tc.os, WorkspacePath: tc.workspace}); got != tc.want {
			t.Errorf("guest root for %s workspace %q = %q, want %q", tc.os, tc.workspace, got, tc.want)
		}
	}
}
