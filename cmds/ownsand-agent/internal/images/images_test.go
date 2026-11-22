// SPDX-License-Identifier: FSL-1.1-ALv2

package images_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func build(t *testing.T, dir, marker string, withMetadata bool) {
	t.Helper()
	b := images.BuildDir(dir)
	if err := os.MkdirAll(b, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "disk"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if withMetadata {
		if err := images.WriteMetadata(b, images.Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, Source: marker}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallReplacesTheImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	build(t, dir, "one", true)
	if err := images.Install(dir); err != nil {
		t.Fatal(err)
	}
	build(t, dir, "two", true)
	if err := images.Install(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "disk")); string(b) != "two" {
		t.Fatalf("installed image %q", b)
	}
	m, err := images.ReadMetadata(dir)
	if err != nil || m.Source != "two" || m.GuestOS != ownsandv1.GuestOs_GUEST_OS_MACOS {
		t.Fatalf("metadata %+v %v", m, err)
	}
	for _, p := range []string{images.BuildDir(dir), dir + ".old"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
}

func TestInstallKeepsTheOldImageWithoutMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), images.WindowsDir)
	build(t, dir, "one", true)
	if err := images.Install(dir); err != nil {
		t.Fatal(err)
	}
	build(t, dir, "partial", false)
	if err := images.Install(dir); err == nil {
		t.Fatal("a build without metadata was installed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "disk")); string(b) != "one" {
		t.Fatalf("installed image %q", b)
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := images.WriteMetadata(dir, images.Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, Source: marker}); err != nil {
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
	if err := images.Install(dir); err == nil {
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
	if err := images.Install(dir); err != nil {
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
	err := images.Install(dir)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("Install = %v", err)
	}
	if exists(dir) || diskOf(t, dir+".old") != "old" {
		t.Fatal("the old image is not kept in the backup")
	}

	// The next call recovers it; the retry then publishes the new image.
	if err := images.Install(dir); err != nil {
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
	err := images.Install(dir)
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
	if err := images.Install(dir); err != nil {
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
