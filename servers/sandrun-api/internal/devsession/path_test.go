// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestResolvePath(t *testing.T) {
	t.Parallel()

	const (
		windows   = sandrunv1.GuestOs_GUEST_OS_WINDOWS
		linux     = sandrunv1.GuestOs_GUEST_OS_LINUX
		macOS     = sandrunv1.GuestOs_GUEST_OS_MACOS
		workspace = `C:\work`
	)
	tests := []struct {
		name      string
		guest     sandrunv1.GuestOs
		workspace string
		path      string
		want      string
	}{
		{"UNC forward", windows, workspace, "//buildhost/share/results.txt", "//buildhost/share/results.txt"},
		{"UNC forward then back", windows, workspace, `/\buildhost/share/results.txt`, `/\buildhost/share/results.txt`},
		{"UNC back then forward", windows, workspace, `\/buildhost/share/results.txt`, `\/buildhost/share/results.txt`},
		{"UNC back", windows, workspace, `\\buildhost\share\results.txt`, `\\buildhost\share\results.txt`},
		{"Windows relative", windows, workspace, "results/output.txt", `C:\work\results\output.txt`},
		{"Windows drive-relative", windows, workspace, "D:results.txt", `C:\work\D:results.txt`},
		{"Windows single forward slash", windows, workspace, "/results.txt", `C:\work\\results.txt`},
		{"Windows single backslash", windows, workspace, `\results.txt`, `C:\work\\results.txt`},
		{"Windows drive with forward slash", windows, workspace, "D:/results/output.txt", "D:/results/output.txt"},
		{"Windows drive with backslash", windows, workspace, `d:\results/output.txt`, `d:\results/output.txt`},
		{"Windows empty", windows, workspace, "", workspace},
		{"Linux absolute", linux, "/work", "/var/results.txt", "/var/results.txt"},
		{"Linux relative", linux, "/work", "results.txt", "/work/results.txt"},
		{"macOS backslashes are relative", macOS, "/work", `\\buildhost\share`, `/work/\\buildhost\share`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePath(tc.guest, tc.workspace, tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("resolvePath(%q) = %q, %v, want %q", tc.path, got, err, tc.want)
			}
		})
	}

	for _, path := range []string{"//buildhost/share/\xff", "//buildhost/share/\x00"} {
		if _, err := resolvePath(windows, workspace, path); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("resolvePath(%q) returned %v, want invalid_argument", path, err)
		}
	}
}
