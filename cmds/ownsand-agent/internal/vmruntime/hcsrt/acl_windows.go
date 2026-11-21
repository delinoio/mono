// SPDX-License-Identifier: Apache-2.0

package hcsrt

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// virtualMachinesSID is the `NT VIRTUAL MACHINE\Virtual Machines` group.
// The Hyper-V worker process of each VM runs as a virtual account in this
// group, and opens the kernel, the disk and the files of the VM directory
// with that identity, not with the agent's. hcsshim grants the same group
// access in its internal security package (GrantVmGroupAccess).
const virtualMachinesSID = "S-1-5-83-0"

// Specific file access rights: generic rights in an explicit ACE of an
// existing file are not mapped, so the ACEs carry specific rights.
const (
	fileGenericRead    = 0x00120089
	fileGenericWrite   = 0x00120116
	fileGenericExecute = 0x001200A0
)

// accessRead and accessReadWrite are the rights that the runtime grants.
const (
	accessRead      windows.ACCESS_MASK = fileGenericRead | fileGenericExecute
	accessReadWrite windows.ACCESS_MASK = fileGenericRead | fileGenericWrite | fileGenericExecute
)

// grantVMGroup adds an ACE that grants access to the Virtual Machines
// group to the DACL of path. A directory ACE is inherited by the files that
// are created in it later. A repeated grant merges into the existing ACE.
func grantVMGroup(path string, access windows.ACCESS_MASK, dir bool) error {
	sid, err := windows.StringToSid(virtualMachinesSID)
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
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: access,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
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
