// SPDX-License-Identifier: FSL-1.1-ALv2

package vmacl

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStripVMGroupWrite(t *testing.T) {
	group, err := windows.StringToSid(GroupSID)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mask windows.ACCESS_MASK
		mode windows.ACCESS_MODE
		keep bool
	}{
		{"read and execute", ReadExecute, windows.GRANT_ACCESS, true},
		{"file write", ReadExecute | windows.FILE_GENERIC_WRITE, windows.GRANT_ACCESS, false},
		{"change ACL", ReadExecute | windows.WRITE_DAC, windows.GRANT_ACCESS, false},
		{"generic write", windows.GENERIC_WRITE, windows.GRANT_ACCESS, false},
		{"deny write", windows.FILE_GENERIC_WRITE, windows.DENY_ACCESS, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			addACE(t, path, group, tc.mask, tc.mode, windows.NO_INHERITANCE)
			addACE(t, path, admins, ReadExecute, windows.GRANT_ACCESS, windows.NO_INHERITANCE)
			if !containsACE(t, path, group) || !containsACE(t, path, admins) {
				t.Fatal("fixture lacks the group or Administrators ACE")
			}
			if err := RemoveGroupWrite(path); err != nil {
				t.Fatal(err)
			}
			if got := containsACE(t, path, group); got != tc.keep {
				t.Errorf("group ACE present = %t, want %t", got, tc.keep)
			}
			if !containsACE(t, path, admins) {
				t.Error("lost the unrelated Administrators ACE")
			}
		})
	}
}

func TestRemoveWorkersClearsInheritedEntries(t *testing.T) {
	dir := t.TempDir()
	worker, err := windows.StringToSid("S-1-5-83-1-1-2-3-4")
	if err != nil {
		t.Fatal(err)
	}
	addACE(t, dir, worker, windows.FILE_GENERIC_WRITE, windows.GRANT_ACCESS, windows.OBJECT_INHERIT_ACE)
	disk := filepath.Join(dir, "disk.vhdx")
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !containsACE(t, disk, worker) {
		t.Fatal("disk did not inherit the worker entry")
	}
	if err := RemoveWorkers(disk); err != nil {
		t.Fatal(err)
	}
	if containsACE(t, disk, worker) || !containsACE(t, dir, worker) {
		t.Error("the worker ACE should remain only on the parent directory")
	}
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatalf("write to the disk after removing the worker grant: %v", err)
	}
}

func testDACL(t *testing.T, path string) (*windows.SECURITY_DESCRIPTOR, *windows.ACL) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	return sd, acl
}

func addACE(t *testing.T, path string, sid *windows.SID, mask windows.ACCESS_MASK, mode windows.ACCESS_MODE, flags uint32) {
	t.Helper()
	sd, old := testDACL(t, path)
	defer runtime.KeepAlive(sd)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        mode,
		Inheritance:       flags,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func containsACE(t *testing.T, path string, sid *windows.SID) bool {
	t.Helper()
	sd, acl := testDACL(t, path)
	defer runtime.KeepAlive(sd)
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if trustee := aceSID(ace); trustee != nil && trustee.Equals(sid) {
			return true
		}
	}
	return false
}
