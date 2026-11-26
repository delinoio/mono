// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"slices"
	"testing"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestMkdirCommand(t *testing.T) {
	t.Run("Windows", func(t *testing.T) {
		got := MkdirCommand(sandrunv1.GuestOs_GUEST_OS_WINDOWS, `C:\Users\O’Brien\ws`)
		want := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			`New-Item -ItemType Directory -Force -Path 'C:\Users\O’’Brien\ws' | Out-Null`}
		if !slices.Equal(got, want) {
			t.Fatalf("MkdirCommand = %q, want %q", got, want)
		}
	})

	t.Run("Linux", func(t *testing.T) {
		const path = "/home/O’Brien/ws"
		got := MkdirCommand(sandrunv1.GuestOs_GUEST_OS_LINUX, path)
		want := []string{"mkdir", "-p", "--", path}
		if !slices.Equal(got, want) {
			t.Fatalf("MkdirCommand = %q, want %q", got, want)
		}
	})
}
