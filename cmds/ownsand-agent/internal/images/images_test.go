// SPDX-License-Identifier: FSL-1.1-ALv2

package images_test

import (
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
