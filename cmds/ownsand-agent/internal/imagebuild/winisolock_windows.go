// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lockWindowsISO(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return nil, fmt.Errorf("find ProgramData: %w", err)
	}
	dir := filepath.Join(programData, "OwnSand-ImageBuildLocks")
	directory, err := openISOLockDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("open the ISO lock directory: %w", err)
	}
	defer windows.CloseHandle(directory)
	return lockISOFile(ctx, dir, path)
}

// openISOLockDirectory gives elevated builders the same directory across
// sessions and users. Reject pre-existing directories with insecure ownership
// or permissions. ProgramData can permit ordinary users to create children.
func openISOLockDirectory(dir string) (windows.Handle, error) {
	const sddl = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	expected, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: expected,
	}
	if err := windows.CreateDirectory(name, &attributes); err != nil && !errors.Is(err, fs.ErrExist) {
		return 0, err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	if err := checkISOLockDirectory(handle, expected); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func checkISOLockDirectory(handle windows.Handle, expected *windows.SECURITY_DESCRIPTOR) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("the ISO lock directory must be a regular directory")
	}
	actual, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if !matchesISOLockDirectoryACL(actual, expected) {
		return errors.New("the ISO lock directory must be owned by Administrators and grant access only to SYSTEM and Administrators")
	}
	return nil
}

func matchesISOLockDirectoryACL(actual, expected *windows.SECURITY_DESCRIPTOR) bool {
	owner, _, err := actual.Owner()
	wantOwner, _, _ := expected.Owner()
	if err != nil || owner == nil || !owner.Equals(wantOwner) {
		return false
	}
	control, _, err := actual.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	acl, _, err := actual.DACL()
	wantACL, _, _ := expected.DACL()
	if err != nil || acl == nil || acl.AceCount != wantACL.AceCount {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var entry, want *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &entry) != nil || windows.GetAce(wantACL, i, &want) != nil ||
			entry.Header != want.Header || entry.Mask != want.Mask ||
			!(*windows.SID)(unsafe.Pointer(&entry.SidStart)).Equals((*windows.SID)(unsafe.Pointer(&want.SidStart))) {
			return false
		}
	}
	return true
}

func lockISOFile(ctx context.Context, dir, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(isoLockKey(path)))
	name, err := windows.UTF16PtrFromString(filepath.Join(dir, fmt.Sprintf("%x.lock", key)))
	if err != nil {
		return nil, err
	}
	// Keep the file after release so all processes continue to lock the same
	// file. Omit delete sharing while any builder has the file open.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("open the lock file: %w", err)
	}
	const pollInterval = 50 * time.Millisecond
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	for {
		var offset windows.Overlapped
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &offset)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &windows.Overlapped{})
				_ = windows.CloseHandle(handle)
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			windows.CloseHandle(handle)
			return nil, fmt.Errorf("lock the file: %w", err)
		}
		select {
		case <-ctx.Done():
			windows.CloseHandle(handle)
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
}
