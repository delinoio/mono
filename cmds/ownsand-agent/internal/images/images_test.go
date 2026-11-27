// SPDX-License-Identifier: FSL-1.1-ALv2

package images_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	macOS   = ownsandv1.GuestOs_GUEST_OS_MACOS
	windows = ownsandv1.GuestOs_GUEST_OS_WINDOWS
)

var macOSFiles = []string{images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel}

func TestInstallReplacesTheImage(t *testing.T) {
	for _, tt := range []struct {
		guestOS ownsandv1.GuestOs
		files   []string
	}{
		{macOS, macOSFiles},
		{windows, []string{images.WindowsDisk}},
	} {
		t.Run(tt.guestOS.String(), func(t *testing.T) {
			dir, err := images.Dir(t.TempDir(), tt.guestOS)
			if err != nil {
				t.Fatal(err)
			}
			stage := images.BuildDir(dir)
			backup := images.OldDir(dir)
			writeImageFiles(t, dir, "previous", tt.guestOS, tt.files)
			writeImageFiles(t, backup, "stale", tt.guestOS, tt.files)
			writeImageFiles(t, stage, "next", tt.guestOS, tt.files)

			if err := images.Install(t.Context(), dir, tt.guestOS); err != nil {
				t.Fatal(err)
			}
			assertImageFiles(t, dir, "next", tt.guestOS, tt.files)
			assertAbsent(t, stage)
			assertAbsent(t, backup)
		})
	}
}

func TestInstallRejectsInvalidStaging(t *testing.T) {
	for _, tt := range []struct {
		guestOS      ownsandv1.GuestOs
		files        []string
		requiredFile string
		otherOS      ownsandv1.GuestOs
		otherFiles   []string
	}{
		{macOS, macOSFiles, images.MacOSAuxStorage, windows, []string{images.WindowsDisk}},
		{windows, []string{images.WindowsDisk}, images.WindowsDisk, macOS, macOSFiles},
	} {
		t.Run(tt.guestOS.String(), func(t *testing.T) {
			for _, fault := range []struct {
				name      string
				wantError string
				inject    func(*testing.T, string)
			}{
				{"missing metadata", images.MetadataFile, func(t *testing.T, stage string) {
					if err := os.Remove(filepath.Join(stage, images.MetadataFile)); err != nil {
						t.Fatal(err)
					}
				}},
				{"malformed metadata", "parse " + images.MetadataFile, func(t *testing.T, stage string) {
					if err := os.WriteFile(filepath.Join(stage, images.MetadataFile), []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
				}},
				{"missing required file", tt.requiredFile, func(t *testing.T, stage string) {
					if err := os.Remove(filepath.Join(stage, tt.requiredFile)); err != nil {
						t.Fatal(err)
					}
				}},
				{"directory in place of required file", tt.requiredFile + " is not a regular file", func(t *testing.T, stage string) {
					path := filepath.Join(stage, tt.requiredFile)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}},
				{"wrong guest OS", "declares guest OS " + tt.otherOS.String() + ", want " + tt.guestOS.String(), func(t *testing.T, stage string) {
					if err := os.RemoveAll(stage); err != nil {
						t.Fatal(err)
					}
					writeImageFiles(t, stage, "wrong OS", tt.otherOS, tt.otherFiles)
				}},
			} {
				t.Run(fault.name, func(t *testing.T) {
					dir, err := images.Dir(t.TempDir(), tt.guestOS)
					if err != nil {
						t.Fatal(err)
					}
					stage := images.BuildDir(dir)
					backup := images.OldDir(dir)
					writeImageFiles(t, dir, "previous", tt.guestOS, tt.files)
					writeImageFiles(t, backup, "stale", tt.guestOS, tt.files)
					writeImageFiles(t, stage, "next", tt.guestOS, tt.files)
					fault.inject(t, stage)

					err = images.Install(t.Context(), dir, tt.guestOS)
					if err == nil || !strings.Contains(err.Error(), fault.wantError) {
						t.Fatalf("Install = %v, want an error about %q", err, fault.wantError)
					}
					assertImageFiles(t, dir, "previous", tt.guestOS, tt.files)
					assertImageFiles(t, backup, "stale", tt.guestOS, tt.files)
				})
			}
		})
	}
}

func TestInstallRejectsIncompleteStagingWithoutACanonicalImage(t *testing.T) {
	for _, tt := range []struct {
		guestOS ownsandv1.GuestOs
		files   []string
	}{
		{macOS, macOSFiles},
		{windows, []string{images.WindowsDisk}},
	} {
		t.Run(tt.guestOS.String(), func(t *testing.T) {
			dir, err := images.Dir(t.TempDir(), tt.guestOS)
			if err != nil {
				t.Fatal(err)
			}
			stage := images.BuildDir(dir)
			writeImageFiles(t, stage, "next", tt.guestOS, tt.files)
			if err := os.Remove(filepath.Join(stage, tt.files[0])); err != nil {
				t.Fatal(err)
			}

			if err := images.Install(t.Context(), dir, tt.guestOS); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("first Install = %v, want a missing file error", err)
			}
			assertAbsent(t, dir)

			backup := images.OldDir(dir)
			writeImageFiles(t, backup, "previous", tt.guestOS, tt.files)
			if err := images.Install(t.Context(), dir, tt.guestOS); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Install after interruption = %v, want a missing file error", err)
			}
			assertImageFiles(t, dir, "previous", tt.guestOS, tt.files)
			assertAbsent(t, backup)
		})
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat %s = %v, want os.ErrNotExist", path, err)
	}
}

func assertImageFiles(t *testing.T, dir, marker string, guestOS ownsandv1.GuestOs, files []string) {
	t.Helper()
	metadata, err := images.ReadMetadata(dir)
	if err != nil || metadata.Source != marker || metadata.GuestOS != guestOS {
		t.Fatalf("metadata in %s = %+v, %v, want source %q and guest OS %v", dir, metadata, err, marker, guestOS)
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil || string(b) != marker {
			t.Fatalf("%s = %q, %v, want %q", path, b, err, marker)
		}
	}
}

func TestDir(t *testing.T) {
	for os, want := range map[ownsandv1.GuestOs]string{
		ownsandv1.GuestOs_GUEST_OS_LINUX:   images.LinuxDir,
		ownsandv1.GuestOs_GUEST_OS_MACOS:   images.MacOSDir,
		ownsandv1.GuestOs_GUEST_OS_WINDOWS: images.WindowsDir,
	} {
		if got, err := images.Dir("/s", os); err != nil || got != filepath.Join("/s", want) {
			t.Fatalf("Dir(%v) = %q %v", os, got, err)
		}
	}
	if _, err := images.Dir("/s", ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED); err == nil {
		t.Fatal("unspecified guest OS has an image directory")
	}
}

// writeImage writes a complete macOS image whose disk holds marker into dir
// (dir itself, not its build directory).
func writeImage(t *testing.T, dir, marker string) {
	t.Helper()
	writeImageFiles(t, dir, marker, macOS, macOSFiles)
}

func writeImageFiles(t *testing.T, dir, marker string, guestOS ownsandv1.GuestOs, files []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := images.WriteMetadata(dir, images.Metadata{GuestOS: guestOS, Source: marker}); err != nil {
		t.Fatal(err)
	}
}

func diskOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, images.MacOSDisk))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestCompleteNeedsTheFilesOfTheGuestOS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	if images.Complete(dir) {
		t.Fatal("a missing directory is complete")
	}
	writeImage(t, dir, "one")
	if !images.Complete(dir) {
		t.Fatal("a complete macOS image is not complete")
	}
	if err := os.Remove(filepath.Join(dir, images.MacOSAuxStorage)); err != nil {
		t.Fatal(err)
	}
	if images.Complete(dir) {
		t.Fatal("an image without aux.img is complete")
	}
	win := filepath.Join(t.TempDir(), images.WindowsDir)
	if err := os.MkdirAll(win, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := images.WriteMetadata(win, images.Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS}); err != nil {
		t.Fatal(err)
	}
	if images.Complete(win) {
		t.Fatal("a Windows image without disk.vhdx is complete")
	}
	if err := os.WriteFile(filepath.Join(win, images.WindowsDisk), []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !images.Complete(win) {
		t.Fatal("a complete Windows image is not complete")
	}
}

func TestMacOSImageRecoveryRejectsLinkedStorage(t *testing.T) {
	for _, name := range []string{images.MacOSDisk, images.MacOSAuxStorage} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, images.MacOSDir)
			writeImage(t, dir, "current")
			file := filepath.Join(dir, name)
			target := filepath.Join(root, "target")
			if err := os.Rename(file, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, file); err != nil {
				t.Skipf("cannot create a symlink on this system: %v", err)
			}
			if images.Complete(dir) {
				t.Fatalf("image with linked %s is complete", name)
			}
			if res, err := images.Recover(dir); err != nil || res != images.RecoverNoBackup {
				t.Fatalf("Recover without backup = %v, %v", res, err)
			}

			writeImage(t, images.OldDir(dir), "backup")
			if res, err := images.Recover(dir); err != nil || res != images.RecoverRestored {
				t.Fatalf("Recover with backup = %v, %v", res, err)
			}
			if got := diskOf(t, dir); got != "backup" || !images.Complete(dir) || exists(images.OldDir(dir)) {
				t.Fatalf("backup was not installed as a complete image: disk marker %q", got)
			}
			if got, err := os.Readlink(filepath.Join(dir+".invalid", name)); err != nil || got != target {
				t.Fatalf("moved invalid image link = %q, %v, want %q", got, err, target)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "current" {
				t.Fatalf("linked target = %q, %v, want unchanged contents", got, err)
			}
		})
	}
}

// Layout 1: the process ended between the two renames of Install, so dir is
// missing, the previous image is in dir.old and the new one is still in
// dir.build.
func TestRecoverRestoresTheBackupAfterAnInterruptedExchange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir+".old", "old")
	writeImage(t, images.BuildDir(dir), "new")

	res, err := images.Recover(dir)
	if err != nil || res != images.RecoverRestored {
		t.Fatalf("Recover = %v %v", res, err)
	}
	if got := diskOf(t, dir); got != "old" {
		t.Fatalf("restored image %q", got)
	}
	if exists(dir + ".old") {
		t.Fatal("the backup is still there")
	}
	if got := diskOf(t, images.BuildDir(dir)); got != "new" {
		t.Fatalf("Recover touched the build directory: %q", got)
	}
	if res, err := images.Recover(dir); err != nil || res != images.RecoverNone {
		t.Fatalf("second Recover = %v %v", res, err)
	}
}

// Layout 2: a retry after the interruption. The first Install call keeps the
// only complete generation when it fails and publishes the new one when it
// succeeds.
func TestInstallAfterAnInterruptedExchangeKeepsAndThenReplacesTheOnlyImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir+".old", "old")
	// A build without image.json cannot be installed.
	if err := os.MkdirAll(images.BuildDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := images.Install(t.Context(), dir, macOS); err == nil {
		t.Fatal("a build without metadata was installed")
	}
	if got := diskOf(t, dir); got != "old" {
		t.Fatalf("a failed retry lost the only complete image: %q", got)
	}
	if err := os.RemoveAll(images.BuildDir(dir)); err != nil {
		t.Fatal(err)
	}

	// A crash again, then a later successful install.
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	writeImage(t, images.BuildDir(dir), "new")
	if err := images.Install(t.Context(), dir, macOS); err != nil {
		t.Fatal(err)
	}
	if got := diskOf(t, dir); got != "new" {
		t.Fatalf("installed image %q", got)
	}
	for _, p := range []string{images.BuildDir(dir), dir + ".old"} {
		if exists(p) {
			t.Fatalf("%s left behind", p)
		}
	}
}

func TestInstallCancellationAroundExchange(t *testing.T) {
	for _, afterRecovery := range []bool{true, false} {
		name := "after exchange starts"
		if afterRecovery {
			name = "after recovery"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), images.MacOSDir)
			old := images.OldDir(dir)
			from, to := dir, old
			if afterRecovery {
				from, to = old, dir
			}
			writeImage(t, from, "previous")
			writeImage(t, images.BuildDir(dir), "staged")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t.Cleanup(images.SetRename(func(src, dst string) error {
				err := os.Rename(src, dst)
				if err == nil && src == from && dst == to {
					cancel()
				}
				return err
			}))
			err := images.Install(ctx, dir, macOS)
			if ctx.Err() != context.Canceled {
				t.Fatal("the fixture did not cancel at the expected rename")
			}
			if afterRecovery {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("install error after recovery = %v, want context.Canceled", err)
				}
				if got := diskOf(t, dir); got != "previous" {
					t.Errorf("installed disk = %q, want previous", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("install error after exchange starts = %v, want nil", err)
			}
			if got := diskOf(t, dir); got != "staged" {
				t.Errorf("installed disk = %q, want staged", got)
			}
		})
	}
}

func TestInstallKeepsTheOldImageWhenThePublishAndTheRestoreFail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir, "old")
	writeImage(t, images.BuildDir(dir), "new")

	// Fail the rename of the build directory (a failed install) and the
	// rename back (as if the process ended there): the backup keeps the image.
	injected := errors.New("injected rename failure")
	restore := images.SetRename(func(from, to string) error {
		if from == images.BuildDir(dir) || from == dir+".old" {
			return injected
		}
		return os.Rename(from, to)
	})
	err := images.Install(t.Context(), dir, macOS)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("Install = %v", err)
	}
	if exists(dir) || diskOf(t, dir+".old") != "old" {
		t.Fatal("the old image is not kept in the backup")
	}

	// The next call recovers it; the retry then publishes the new image.
	if err := images.Install(t.Context(), dir, macOS); err != nil {
		t.Fatal(err)
	}
	if got := diskOf(t, dir); got != "new" {
		t.Fatalf("installed image %q", got)
	}
}

func TestInstallRestoresTheOldImageWhenOnlyThePublishFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir, "old")
	writeImage(t, images.BuildDir(dir), "new")
	injected := errors.New("injected rename failure")
	restore := images.SetRename(func(from, to string) error {
		if from == images.BuildDir(dir) {
			return injected
		}
		return os.Rename(from, to)
	})
	err := images.Install(t.Context(), dir, macOS)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("Install = %v", err)
	}
	if got := diskOf(t, dir); got != "old" {
		t.Fatalf("image after a failed install %q", got)
	}
}

// Layout 3: an unfinished build directory is never a recovery source.
func TestRecoverNeverConsumesTheBuildDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, images.BuildDir(dir), "unfinished")
	res, err := images.Recover(dir)
	if err != nil || res != images.RecoverNoBackup {
		t.Fatalf("Recover = %v %v", res, err)
	}
	if exists(dir) || diskOf(t, images.BuildDir(dir)) != "unfinished" {
		t.Fatal("Recover used the build directory")
	}
	// An incomplete backup is not a source either.
	if err := os.MkdirAll(dir+".old", 0o700); err != nil {
		t.Fatal(err)
	}
	if res, err := images.Recover(dir); err != nil || res != images.RecoverNoBackup {
		t.Fatalf("Recover with an empty backup = %v %v", res, err)
	}
	if exists(dir) {
		t.Fatal("Recover restored an incomplete backup")
	}
}

// Layout 4: a complete canonical image wins over a leftover backup.
func TestRecoverPrefersAValidCanonicalImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir, "current")
	writeImage(t, dir+".old", "stale")
	res, err := images.Recover(dir)
	if err != nil || res != images.RecoverNone {
		t.Fatalf("Recover = %v %v", res, err)
	}
	if got := diskOf(t, dir); got != "current" {
		t.Fatalf("image %q", got)
	}
	if got := diskOf(t, dir+".old"); got != "stale" {
		t.Fatal("Recover changed the backup")
	}
	// The next Install removes the leftover.
	writeImage(t, images.BuildDir(dir), "new")
	if err := images.Install(t.Context(), dir, macOS); err != nil {
		t.Fatal(err)
	}
	if exists(dir + ".old") {
		t.Fatal("the stale backup is still there")
	}
}

func TestRecoverReplacesAnInvalidCanonicalDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, images.MacOSDisk), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeImage(t, dir+".old", "old")
	if res, err := images.Recover(dir); err != nil || res != images.RecoverRestored {
		t.Fatalf("Recover = %v %v", res, err)
	}
	if got := diskOf(t, dir); got != "old" {
		t.Fatalf("image %q", got)
	}
	if got := diskOf(t, dir+".invalid"); got != "partial" {
		t.Fatalf("the invalid directory was not kept: %q", got)
	}
}

func TestRecoverWaitsForARunningInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir+".old", "old")
	lock, err := images.TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := images.Recover(dir)
	if err != nil || res != images.RecoverDeferred {
		t.Fatalf("Recover under the lock = %v %v", res, err)
	}
	if exists(dir) {
		t.Fatal("Recover exchanged directories while another handle held the lock")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if res, err := images.Recover(dir); err != nil || res != images.RecoverRestored {
		t.Fatalf("Recover after the release = %v %v", res, err)
	}
}

func TestRecoverCreatesNoLockFileWhenNothingIsToRestore(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, images.MacOSDir)
	writeImage(t, dir, "current")
	if _, err := images.Recover(dir); err != nil {
		t.Fatal(err)
	}
	if exists(images.LockPath(dir)) {
		t.Fatal("a plain read created a lock file")
	}
}

func TestResolveRecoversAndReadsTheMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	writeImage(t, dir+".old", "old")
	m, err := images.Resolve(dir)
	if err != nil || m.Source != "old" {
		t.Fatalf("Resolve = %+v %v", m, err)
	}
	if _, err := images.Resolve(filepath.Join(t.TempDir(), images.WindowsDir)); err == nil {
		t.Fatal("Resolve of a missing image succeeded")
	}
}
