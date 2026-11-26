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

func TestGuestCommandsAndPathsResolve(t *testing.T) {
	windows := &sandrunv1.DevSession{GuestOs: sandrunv1.GuestOs_GUEST_OS_WINDOWS, WorkspacePath: `C:\work`}
	linux := &sandrunv1.DevSession{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/work"}
	for _, tc := range []struct {
		name    string
		session *sandrunv1.DevSession
		path    string
		want    string
	}{
		{"UNC backslashes", windows, `\\buildhost\share\results.txt`, `\\buildhost\share\results.txt`},
		{"UNC forward slashes", windows, "//buildhost/share/results.txt", "//buildhost/share/results.txt"},
		{"UNC forward slash then backslash", windows, `/\buildhost/share/results.txt`, `/\buildhost/share/results.txt`},
		{"UNC backslash then forward slash", windows, `\/buildhost/share/results.txt`, `\/buildhost/share/results.txt`},
		{"drive root", windows, "D:/out/results.txt", "D:/out/results.txt"},
		{"Windows workspace relative", windows, "out/results.txt", `C:\work\out\results.txt`},
		{"single forward slash", windows, "/out/results.txt", `C:\work\out\results.txt`},
		{"single backslash", windows, `\out\results.txt`, `C:\work\\out\results.txt`},
		{"POSIX absolute", linux, "/var/results.txt", "/var/results.txt"},
		{"POSIX workspace relative", linux, "results.txt", "/work/results.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := guestResolve(tc.session, tc.path); got != tc.want {
				t.Errorf("guestResolve(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
