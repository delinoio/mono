// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package credstore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLockPermissionsAcrossProcesses(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	run := func(action string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCredentialMutation$")
		cmd.Env = append(os.Environ(), envHelperDir+"="+s.Dir, "CREDSTORE_TEST_MUTATION="+action)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", action, err, out)
		}
	}
	lockPath := filepath.Join(s.Dir, LockFileName)
	run("save")
	original := requireFileMode(t, lockPath, 0o600)
	requireFileMode(t, s.Path(), 0o600)
	run("rotate")
	credentials, err := s.Load()
	if err != nil || credentials.RefreshToken != "r1" {
		t.Fatal("refresh did not publish the rotated token")
	}
	run("save")
	requireFileMode(t, s.Path(), 0o600)
	run("delete")
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete: %v", err)
	}
	if !os.SameFile(original, requireFileMode(t, lockPath, 0o600)) {
		t.Fatal("credential mutations replaced the persistent lock")
	}
}

func TestHelperCredentialMutation(t *testing.T) {
	action := os.Getenv("CREDSTORE_TEST_MUTATION")
	if action == "" {
		t.Skip("helper process only")
	}
	s := Store{Dir: os.Getenv(envHelperDir)}
	if s.Dir == "" {
		t.Fatal("missing store directory")
	}
	defer syscall.Umask(syscall.Umask(0o022))
	var err error
	switch action {
	case "save":
		syscall.Umask(0o200)
		err = s.Save(&Credentials{SessionID: "s", RefreshToken: "r0"})
	case "rotate":
		err = s.SaveIfCurrent("s", "r0", &Credentials{SessionID: "s", RefreshToken: "r1"})
	case "delete":
		err = s.Delete()
	default:
		t.Fatalf("unknown mutation %q", action)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestRepairLockPreservesInodeAndExclusion(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, LockFileName)
	original := requireFileMode(t, path, 0o600)
	release := startLockHelper(t, s.Dir, false)
	for _, mode := range []os.FileMode{0o000, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		f, err := openLockFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := tryLockFile(f)
		_ = f.Close()
		if err != nil || ok {
			t.Fatalf("tryLockFile during repair at mode %04o: acquired=%v, err=%v", mode, ok, err)
		}
		if !os.SameFile(original, requireFileMode(t, path, 0o600)) {
			t.Fatal("permission repair replaced the lock")
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if !contenderAcquires(t, s.Dir) {
		t.Fatal("lock remained held after helper release")
	}
}

func TestLockRejectsSymlink(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	target := filepath.Join(s.Dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.Dir, LockFileName)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Credentials{RefreshToken: "r"}); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Save with a symlink as the lock: %v", err)
	}
	requireFileMode(t, target, 0o400)
}

func requireFileMode(t *testing.T, path string, mode os.FileMode) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode(); got != mode {
		t.Fatalf("%s mode = %v, want %v", filepath.Base(path), got, mode)
	}
	return info
}
