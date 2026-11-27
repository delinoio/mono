// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmacl"
)

// Windows ignores POSIX modes, so the state directory is protected with an
// explicit DACL. The state directory lives under %ProgramData%, where the
// default ACL lets every local user create entries and read what others
// create. The DACL of the state directory root is protected (it does not
// inherit from %ProgramData%) and holds only two inheritable entries, for
// NT AUTHORITY\SYSTEM and BUILTIN\Administrators, so every file and
// directory that the agent creates later inherits them.
//
// Hyper-V workers reach explicitly granted files below the protected state
// directory through the default SeChangeNotifyPrivilege, which bypasses
// traverse checks on the parent directories.

const (
	// fileAllAccess is FILE_ALL_ACCESS. An explicit ACE on an existing
	// object carries specific rights because generic rights are not mapped.
	fileAllAccess windows.ACCESS_MASK = 0x001F01FF
	// inheritFlags makes an ACE apply to the directory, its subdirectories
	// and its files. It equals OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE.
	inheritFlags = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	// aceInheritMask selects the inheritance bits of ACE_HEADER.AceFlags.
	aceInheritMask = 0x0F
)

// principals holds the SIDs that the restriction handles.
type principals struct {
	system  *windows.SID
	admins  *windows.SID
	vmGroup *windows.SID
	trusted []*windows.SID // SYSTEM and Administrators: the entries of the DACL
	// rootOwners and childOwners are the SIDs that may own the state
	// directory and an entry below it. They add the user of an elevated
	// administrator process as an owner only (see processIdentity).
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
	vm, err := windows.StringToSid(vmacl.GroupSID)
	if err != nil {
		return nil, fmt.Errorf("resolve the Virtual Machines SID: %w", err)
	}
	p := &principals{
		system:  system,
		admins:  admins,
		vmGroup: vm,
		trusted: []*windows.SID{system, admins},
	}
	p.rootOwners = append([]*windows.SID{}, p.trusted...)
	p.childOwners = []*windows.SID{system, admins, vm}
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

// resetChildren checks the owners and ACLs of existing state files.
func resetChildren(root string, p *principals) error {
	vmPrefix := strings.ToLower(filepath.Join(root, VMsDir)) + string(os.PathSeparator)
	winImage := filepath.Join(root, ImagesDir, images.WindowsDir)
	buildDir := images.BuildDir(winImage)
	var buildLock *images.Lock
	var activeBuildDisk string
	defer func() {
		if buildLock != nil {
			if err := buildLock.Release(); err != nil {
				slog.Warn("release the Windows image build lock failed", "image_dir", winImage, "error", err)
			}
		}
	}()
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
		if d.IsDir() && strings.EqualFold(path, buildDir) {
			// The builder holds this lock from before staging until after
			// installation. Preserve its live disk grant if it is running.
			lockPath := images.LockPath(winImage)
			if info, err := os.Lstat(lockPath); err == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("the Windows image build lock is not a regular file: %s", lockPath)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			lock, err := images.TryLock(winImage)
			switch {
			case errors.Is(err, images.ErrLocked):
				activeBuildDisk = filepath.Join(path, images.WindowsDisk)
			case err != nil:
				return fmt.Errorf("check the Windows image build lock: %w", err)
			default:
				buildLock = lock
			}
		}
		keepWorker := strings.HasPrefix(strings.ToLower(path), vmPrefix) ||
			strings.EqualFold(path, activeBuildDisk)
		if err := resetChild(path, p, keepWorker); err != nil {
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

func (p *principals) keepACE(a aceInfo, keepWorker bool) bool {
	if a.aceType != windows.ACCESS_ALLOWED_ACE_TYPE || a.sid == nil {
		return false
	}
	if sidIn(a.sid, p.trusted) {
		return true
	}
	switch vmacl.Classify(a.sid) {
	case vmacl.Worker:
		return keepWorker
	case vmacl.WorkerGroup:
		return a.flags&aceInheritMask == 0 && vmacl.ReadOnly(a.mask)
	default:
		return false
	}
}

func resetChild(path string, p *principals, keepWorker bool) error {
	// Hyper-V can create files in the name of the Virtual Machines group.
	if err := checkOwner(path, p, p.childOwners); err != nil {
		return err
	}
	aces, protected, nullDACL, release, err := readDACL(path)
	if err != nil {
		return err
	}
	defer release()
	dirty := protected || nullDACL
	var keep []windows.EXPLICIT_ACCESS
	var removedGroup bool
	for _, a := range aces {
		if !p.keepACE(a, keepWorker) {
			dirty = true
			removedGroup = removedGroup || vmacl.Classify(a.sid) == vmacl.WorkerGroup
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
	if removedGroup {
		slog.Info("removed Hyper-V worker group access entry", "path", path, "sid", vmacl.GroupSID)
	}
	return nil
}
