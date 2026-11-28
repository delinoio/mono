// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func requireStatusFixturePrivileges(t *testing.T) {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("read the process token user: %v", err)
	}
	if user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return
	}

	// Token.IsElevated hides errors. Read TokenElevation directly so a
	// failed query fails the test instead of skipping it.
	var elevation, size uint32
	const elevationSize = uint32(unsafe.Sizeof(elevation))
	if err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevation)), elevationSize, &size); err != nil {
		t.Fatalf("read the process token elevation: %v", err)
	}
	if size != elevationSize {
		t.Fatalf("read the process token elevation: got %d bytes, want %d", size, elevationSize)
	}
	if elevation != 0 {
		groups, err := token.GetTokenGroups()
		if err != nil {
			t.Fatalf("read the process token groups: %v", err)
		}
		for _, group := range groups.AllGroups() {
			if group.Attributes&windows.SE_GROUP_ENABLED != 0 && group.Sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
				return
			}
		}
	}
	t.Skip("the writable Windows status fixture requires SYSTEM or an elevated administrator")
}
