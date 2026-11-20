// SPDX-License-Identifier: Apache-2.0

//go:build windows

package credstore

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// userOnlyACL returns a DACL that grants full access to the current user
// only.
func userOnlyACL(inherit uint32) (*windows.ACL, error) {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("credstore: current user: %w", err)
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
}

// setUserOnly replaces the DACL of path with a protected user-only DACL,
// so inherited entries (for example of Administrators) no longer apply.
func setUserOnly(path string, inherit uint32) error {
	acl, err := userOnlyACL(inherit)
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("credstore: restrict %s: %w", path, err)
	}
	return nil
}

func restrictDir(dir string) error {
	return setUserOnly(dir, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
}

func restrictFile(_ *os.File, name string) error {
	return setUserOnly(name, windows.NO_INHERITANCE)
}
