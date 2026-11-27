// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmacl"
)

// grantVM cleans a copied disk and gives its compute system access before
// HCS opens it. HCS derives the worker SID from the compute system ID.
func grantVM(id, disk string) error {
	if err := vmacl.RemoveWorkers(disk); err != nil {
		return err
	}
	if err := procHcsGrantVmAccess.Find(); err != nil {
		return fmt.Errorf("hcsrt: find HcsGrantVmAccess: %w", err)
	}
	vmID, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(disk)
	if err != nil {
		return err
	}
	hr, _, _ := procHcsGrantVmAccess.Call(uintptr(unsafe.Pointer(vmID)), uintptr(unsafe.Pointer(path)))
	if failed(hr) {
		return fmt.Errorf("grant compute system %q access to %s: %w", id, disk, &hcsError{op: "HcsGrantVmAccess", hr: uint32(hr)})
	}
	return nil
}

// The Linux kernel is shared by VM workers and needs only read and execute access.
func grantKernelRead(path string) error {
	if err := vmacl.RemoveGroupWrite(path); err != nil {
		return err
	}
	sid, err := windows.StringToSid(vmacl.GroupSID)
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("hcsrt: read the ACL of %s: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("hcsrt: read the ACL of %s: %w", path, err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: vmacl.ReadExecute,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, dacl)
	if err != nil {
		return fmt.Errorf("hcsrt: build the ACL of %s: %w", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("hcsrt: grant the Hyper-V worker access to %s: %w", path, err)
	}
	return nil
}
