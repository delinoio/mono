// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows ignores POSIX modes, so the state directory is protected with an
// explicit DACL. The state directory lives under %ProgramData%, where the
// default ACL lets every local user create entries and read what others
// create. The DACL of the state directory root is protected (it does not
// inherit from %ProgramData%) and holds only two inheritable entries, for
// NT AUTHORITY\SYSTEM and BUILTIN\Administrators, so every file and
// directory that the agent creates later inherits them.
//
// Hyper-V worker processes run as virtual accounts of the group
// NT VIRTUAL MACHINE\Virtual Machines. The runtime grants that group access
// to each VM directory and VM file it uses with an explicit entry (see
// hcsrt.grantVMGroup). Those entries stay: the restriction below keeps
// explicit entries of that group. The worker reaches the files without
// access to the protected parent directories because Windows bypasses the
// traverse check for an account that holds SeChangeNotifyPrivilege, which
// every account holds by default.

const (
	// fileAllAccess is FILE_ALL_ACCESS. An explicit ACE on an existing
	// object carries specific rights because generic rights are not mapped.
	fileAllAccess windows.ACCESS_MASK = 0x001F01FF
	// inheritFlags makes an ACE apply to the directory, its subdirectories
	// and its files. It equals OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE.
	inheritFlags = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	// aceInheritMask selects the inheritance bits of ACE_HEADER.AceFlags.
	aceInheritMask = 0x0F
	// virtualMachinesSID is the group of the Hyper-V worker processes.
	virtualMachinesSID = "S-1-5-83-0"
)

// principals holds the SIDs that the restriction handles.
type principals struct {
	system   *windows.SID
	admins   *windows.SID
	vmGroup  *windows.SID
	trusted  []*windows.SID // SYSTEM and Administrators: the entries of the DACL
	retained []*windows.SID // SIDs whose explicit ACEs may stay on a child
	// rootOwners and childOwners are the SIDs that may own the state
	// directory and an entry below it. They add the user of an elevated
	// administrator process to trusted and retained (see processIdentity).
	// They never widen a DACL: only trusted and retained select ACEs.
	rootOwners  []*windows.SID
	childOwners []*windows.SID
}

// processIdentity returns the user SID of the process token and whether the
// process is an elevated administrator (or SYSTEM). CheckTokenMembership
// reports a member of Administrators only for an elevated token, because the
// group is deny-only in the filtered token of a non-elevated administrator.
func processIdentity() (user *windows.SID, elevated bool, err error) {
	token := windows.GetCurrentProcessToken()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return nil, false, fmt.Errorf("read the user of the process token: %w", err)
	}
	user, err = tokenUser.User.Sid.Copy()
	if err != nil {
		return nil, false, fmt.Errorf("copy the user of the process token: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, false, fmt.Errorf("resolve the Administrators SID: %w", err)
	}
	member, err := windows.Token(0).IsMember(admins)
	if err != nil {
		return nil, false, fmt.Errorf("check the Administrators membership: %w", err)
	}
	return user, member && token.IsElevated(), nil
}

func loadPrincipals() (*principals, error) {
	user, elevated, err := processIdentity()
	if err != nil {
		return nil, err
	}
	return newPrincipals(user, elevated)
}

// newPrincipals builds the principals for a process whose token user is user.
// An elevated administrator may also own the state directory and its
// entries: it can take ownership of any object anyway, so refusing its own
// objects protects nothing, and it is the account that creates them (the
// owner of an object that an elevated administrator creates is usually the
// Administrators group, but a directory that was made before, such as the
// temporary directory of the account, is owned by the user). A process that
// is not elevated gets no extra owner, so a directory of a standard user, or
// of an administrator whose token is filtered, still fails closed.
func newPrincipals(user *windows.SID, elevated bool) (*principals, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolve the SYSTEM SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("resolve the Administrators SID: %w", err)
	}
	vm, err := windows.StringToSid(virtualMachinesSID)
	if err != nil {
		return nil, fmt.Errorf("resolve the Virtual Machines SID: %w", err)
	}
	p := &principals{
		system:   system,
		admins:   admins,
		vmGroup:  vm,
		trusted:  []*windows.SID{system, admins},
		retained: []*windows.SID{system, admins, vm},
	}
	p.rootOwners = append([]*windows.SID{}, p.trusted...)
	p.childOwners = append([]*windows.SID{}, p.retained...)
	if elevated && user != nil {
		p.rootOwners = append(p.rootOwners, user)
		p.childOwners = append(p.childOwners, user)
	}
	return p, nil
}

func sidIn(sid *windows.SID, set []*windows.SID) bool {
	for _, s := range set {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}

// restrictStateDir protects the state directory root, corrects the ACLs of
// the entries that already exist and fails closed when an entry belongs to
// an untrusted owner (anyone except SYSTEM, Administrators and the user of an
// elevated administrator process). An owner can always change the DACL of its
// own object, so a directory that a standard user created cannot be made
// private.
func restrictStateDir(dir string) error {
	p, err := loadPrincipals()
	if err != nil {
		return err
	}
	if err := checkOwner(dir, p, p.rootOwners); err != nil {
		return err
	}
	if err := protectRoot(dir, p); err != nil {
		return err
	}
	return resetChildren(dir, p)
}

// ownerAllowed reports whether owner is one of the allowed SIDs.
func ownerAllowed(owner *windows.SID, allowed []*windows.SID) bool {
	return owner != nil && sidIn(owner, allowed)
}

// checkOwner fails unless the owner of path is one of the allowed SIDs.
func checkOwner(path string, p *principals, allowed []*windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", path, err)
	}
	if !ownerAllowed(owner, allowed) {
		return fmt.Errorf("%s is owned by %s, but the state directory and its content must be owned by NT AUTHORITY\\SYSTEM, BUILTIN\\Administrators or, in an elevated administrator process, that process's own user; delete it or take its ownership as an administrator", path, describeSID(owner))
	}
	return nil
}

func describeSID(sid *windows.SID) string {
	if sid == nil {
		return "an unknown owner"
	}
	if account, domain, _, err := sid.LookupAccount(""); err == nil {
		return fmt.Sprintf("%s\\%s (%s)", domain, account, sid)
	}
	return sid.String()
}

// aceInfo is one entry of a DACL.
type aceInfo struct {
	aceType   uint8
	flags     uint8
	mask      windows.ACCESS_MASK
	sid       *windows.SID
	inherited bool
}

// readACEs returns the entries of acl. A nil acl is a NULL DACL, which
// grants everyone full access; it has no entries and the caller must treat
// it as unprotected through the returned flag.
func readACEs(acl *windows.ACL) ([]aceInfo, error) {
	if acl == nil {
		return nil, nil
	}
	out := make([]aceInfo, 0, acl.AceCount)
	for i := 0; i < int(acl.AceCount); i++ {
		var a *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &a); err != nil {
			return nil, fmt.Errorf("read ACE %d: %w", i, err)
		}
		info := aceInfo{
			aceType:   a.Header.AceType,
			flags:     a.Header.AceFlags,
			mask:      a.Mask,
			inherited: a.Header.AceFlags&windows.INHERITED_ACE != 0,
		}
		// Only the allowed and denied ACE types share the layout whose SID
		// follows the mask; other types are never accepted.
		if a.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE || a.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			info.sid = (*windows.SID)(unsafe.Pointer(&a.SidStart))
		}
		out = append(out, info)
	}
	return out, nil
}

// readDACL returns the entries of the DACL of path, whether the DACL is
// protected from inheritance, and keeps the security descriptor alive until
// the entries are no longer used through the returned release function.
func readDACL(path string) (aces []aceInfo, protected bool, nullDACL bool, release func(), err error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, false, false, nil, fmt.Errorf("read the ACL of %s: %w", path, err)
	}
	release = func() { runtime.KeepAlive(sd) }
	control, _, err := sd.Control()
	if err != nil {
		return nil, false, false, nil, fmt.Errorf("read the ACL of %s: %w", path, err)
	}
	acl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return nil, false, false, nil, fmt.Errorf("read the ACL of %s: %w", path, err)
	}
	aces, err = readACEs(acl)
	if err != nil {
		return nil, false, false, nil, fmt.Errorf("read the ACL of %s: %w", path, err)
	}
	return aces, control&windows.SE_DACL_PROTECTED != 0, acl == nil, release, nil
}

// rootCompliant reports whether the DACL of the root is exactly the
// protected DACL that protectRoot writes.
func rootCompliant(aces []aceInfo, protected, nullDACL bool, p *principals) bool {
	if !protected || nullDACL || len(aces) != 2 {
		return false
	}
	seenSystem, seenAdmins := false, false
	for _, a := range aces {
		if a.aceType != windows.ACCESS_ALLOWED_ACE_TYPE || a.inherited || a.sid == nil ||
			a.mask != fileAllAccess || a.flags&aceInheritMask != inheritFlags {
			return false
		}
		switch {
		case a.sid.Equals(p.system):
			seenSystem = true
		case a.sid.Equals(p.admins):
			seenAdmins = true
		default:
			return false
		}
	}
	return seenSystem && seenAdmins
}

func trustee(sid *windows.SID) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}

// protectRoot replaces the DACL of the root with the protected DACL.
// SetNamedSecurityInfo propagates the inheritable entries to existing
// children, which replaces the inherited entries of the old parent. The
// DACL is written only when it differs, because a write touches every
// descendant, including the files of a running VM.
func protectRoot(dir string, p *principals) error {
	aces, protected, nullDACL, release, err := readDACL(dir)
	if err != nil {
		return err
	}
	compliant := rootCompliant(aces, protected, nullDACL, p)
	release()
	if compliant {
		return nil
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{AccessPermissions: fileAllAccess, AccessMode: windows.GRANT_ACCESS, Inheritance: inheritFlags, Trustee: trustee(p.system)},
		{AccessPermissions: fileAllAccess, AccessMode: windows.GRANT_ACCESS, Inheritance: inheritFlags, Trustee: trustee(p.admins)},
	}, nil)
	if err != nil {
		return fmt.Errorf("build the ACL of %s: %w", dir, err)
	}
	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("restrict %s to SYSTEM and Administrators (the agent must run as SYSTEM or as an elevated administrator): %w", dir, err)
	}
	return nil
}

// resetChildren walks the existing entries of the state directory. An entry
// that has an untrusted owner stops the walk with an error. An entry whose
// ACL is protected from inheritance, or holds an entry for another account
// than SYSTEM, Administrators or the Virtual Machines group, gets a DACL
// that keeps only its explicit entries of those three accounts and inherits
// the rest from its parent. Explicit entries of the Virtual Machines group
// stay because a running VM needs them.
func resetChildren(root string, p *principals) error {
	// An owner of a child may be the Virtual Machines group too: the group
	// has no logon, but Hyper-V can create files for a VM in its name.
	childOwners := p.childOwners
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An entry that disappears during the walk (a VM file that a
			// runtime deletes) is not an error.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if path == root {
			return nil
		}
		// A link must not redirect the ACL change to another object.
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := resetChild(path, p, childOwners); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil
			}
			return err
		}
		return nil
	})
}

// aclFromEntries builds an ACL from entries. SetEntriesInAcl may return a
// NULL ACL for no entries, and a NULL DACL grants everyone full access, so
// an empty list becomes an explicit empty ACL (revision 2, no entries).
func aclFromEntries(entries []windows.EXPLICIT_ACCESS) (*windows.ACL, error) {
	if len(entries) == 0 {
		empty := make([]byte, 8)
		empty[0] = 2 // ACL_REVISION
		empty[2] = 8 // AclSize
		return (*windows.ACL)(unsafe.Pointer(&empty[0])), nil
	}
	return windows.ACLFromEntries(entries, nil)
}

func resetChild(path string, p *principals, owners []*windows.SID) error {
	if err := checkOwner(path, p, owners); err != nil {
		return err
	}
	aces, protected, nullDACL, release, err := readDACL(path)
	if err != nil {
		return err
	}
	defer release()
	dirty := protected || nullDACL
	var keep []windows.EXPLICIT_ACCESS
	for _, a := range aces {
		allowedSID := a.aceType == windows.ACCESS_ALLOWED_ACE_TYPE && a.sid != nil && sidIn(a.sid, p.retained)
		if !allowedSID {
			dirty = true
			continue
		}
		if !a.inherited {
			keep = append(keep, windows.EXPLICIT_ACCESS{
				AccessPermissions: a.mask,
				AccessMode:        windows.GRANT_ACCESS,
				Inheritance:       uint32(a.flags & aceInheritMask),
				Trustee:           trustee(a.sid),
			})
		}
	}
	if !dirty {
		return nil
	}
	acl, err := aclFromEntries(keep)
	if err != nil {
		return fmt.Errorf("build the ACL of %s: %w", path, err)
	}
	// UNPROTECTED makes Windows add the entries that the parent passes on.
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("reset the ACL of %s: %w", path, err)
	}
	return nil
}
