//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"os/user"
)

// errChmodUnsupported marks the Windows Chmod limitation: Windows has no
// Unix permission bits, so the ACL below restricts the file instead.
var errChmodUnsupported = errors.New("chmod unsupported")

// restrictToUser removes inherited ACL entries from path and grants full
// control to the current user only, with the built-in icacls tool.
//
// Workaround: the standard library cannot set Windows ACLs, and
// golang.org/x/sys/windows is not an approved direct dependency. Scope: the
// credential file and its directory. Remove this when an approved ACL API
// (or the OS keychain storage, an open decision) replaces it. This code is
// cross-compiled and vetted but not yet verified on a Windows host.
func restrictToUser(path string, dir bool) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the current user: %w", err)
	}
	grant := u.Username + ":F"
	if dir {
		// (OI)(CI): new files in the directory inherit the user-only entry.
		grant = u.Username + ":(OI)(CI)F"
	}
	out, err := exec.Command("icacls", path, "/inheritance:r", "/grant:r", grant).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restrict %s to the current user with icacls: %v: %s", path, err, out)
	}
	return nil
}
