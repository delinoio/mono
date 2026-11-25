// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"testing"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

func TestGuestCommandsAndPathsWorkingDirectory(t *testing.T) {
	windows := tidev1.GuestOs_GUEST_OS_WINDOWS
	for _, tc := range []struct {
		os        tidev1.GuestOs
		workspace string
		want      string
	}{
		{tidev1.GuestOs_GUEST_OS_LINUX, "/home/dev/work", "/"},
		{tidev1.GuestOs_GUEST_OS_MACOS, "/Users/dev/work", "/"},
		{windows, `C:\work`, `C:\`},
		{windows, `D:\a\b\`, `D:\`},
		{windows, `c:\work`, `c:\`},
		{windows, `C:work`, ""},
		{windows, `\\host\share\work`, ""},
		{windows, `1:\work`, ""},
		{windows, "", ""},
	} {
		if got := guestRoot(&tidev1.DevSession{GuestOs: tc.os, WorkspacePath: tc.workspace}); got != tc.want {
			t.Errorf("guest root for %s workspace %q = %q, want %q", tc.os, tc.workspace, got, tc.want)
		}
	}
}
