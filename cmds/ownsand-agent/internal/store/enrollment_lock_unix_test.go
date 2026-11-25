// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const enrollmentLockUmaskTestDir = "OWNSAND_ENROLLMENT_LOCK_UMASK_TEST_DIR"

func TestEnrollmentLockRestrictiveUmask(t *testing.T) {
	if dir := os.Getenv(enrollmentLockUmaskTestDir); dir != "" {
		// Change the process-wide umask only in this dedicated subprocess.
		defer unix.Umask(unix.Umask(0o600))
		lockEnrollment(t, openStore(t, dir))()
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnrollmentLockRestrictiveUmask$")
	cmd.Env = append(os.Environ(), enrollmentLockUmaskTestDir+"="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restrictive umask subprocess: %v\n%s", err, output)
	}
	s := openStore(t, dir)
	info, err := os.Stat(s.Path(EnrollmentLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %04o, want 0600", info.Mode().Perm())
	}
	lockEnrollment(t, s)()
}

func TestEnrollmentLockRepairsLegacyMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o000, 0o200, 0o400} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := s.Path(EnrollmentLockFile)
			unlock := lockEnrollment(t, s)
			defer unlock()
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Repair must preserve the lock held on this inode.
			requireBusy(t, openStore(t, s.Dir()))
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode().Perm() != 0o600 || !os.SameFile(before, after) {
				t.Fatalf("repair mode = %04o, same inode = %t", after.Mode().Perm(), os.SameFile(before, after))
			}
			unlock()
			lockEnrollment(t, s)()
		})
	}
}

func TestEnrollmentLockRejectsUnexpectedEntries(t *testing.T) {
	for name, create := range map[string]func(path, target string) error{
		"symlink":    os.Symlink,
		"hardlink":   os.Link,
		"fifo":       func(_, target string) error { return unix.Mkfifo(target, 0o600) },
		"directory":  func(_, target string) error { return os.Mkdir(target, 0o700) },
		"other-mode": func(_, target string) error { return os.WriteFile(target, nil, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, nil, 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o400); err != nil {
				t.Fatal(err)
			}
			path := s.Path(EnrollmentLockFile)
			if err := create(target, path); err != nil {
				t.Fatal(err)
			}
			if name == "other-mode" {
				// Keep the fixture wider than 0600 under the caller's umask.
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := s.LockEnrollment(t.Context())
			if err == nil {
				unlock()
				t.Fatal("acquired lock on unexpected entry")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o400 {
				t.Fatalf("target mode = %04o, want 0400", info.Mode().Perm())
			}
		})
	}
}
