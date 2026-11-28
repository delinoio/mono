// SPDX-License-Identifier: FSL-1.1-ALv2

package images

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestPublicationLockBlocksNewReadersUntilRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), WindowsDir)
	writer, err := tryLock(publicationLockPath(dir), exclusiveLock)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	read := func() error {
		called = true
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := WithBaseImage(ctx, dir, read); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Errorf("read during publication = %v, called = %t", err, called)
	}
	if err := writer.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := WithBaseImage(ctx, dir, read); err != nil || !called {
		t.Fatalf("read after publication = %v, called = %t", err, called)
	}
}

func TestRecoverDefersWhilePublicationHasReaders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), WindowsDir)
	old := OldDir(dir)
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(old, Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, WindowsDisk), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := tryLock(publicationLockPath(dir), sharedLock)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tryLock(publicationLockPath(dir), sharedLock)
	if err != nil {
		_ = reader.Release()
		t.Fatal(err)
	}
	if res, err := Recover(dir); res != RecoverDeferred || err != nil {
		t.Errorf("Recover with two readers = %v, %v", res, err)
	}
	if err := errors.Join(second.Release(), reader.Release()); err != nil {
		t.Fatal(err)
	}
	if res, err := Recover(dir); res != RecoverRestored || err != nil {
		t.Fatalf("Recover after both readers release = %v, %v", res, err)
	}
}

func TestTryWithBaseImageDuringMacOSPublication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), MacOSDir)
	writeCapabilityImage(t, dir, "previous")
	writeCapabilityImage(t, BuildDir(dir), "replacement")

	build, err := TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer build.releaseAndLog()

	paused, resume := make(chan struct{}), make(chan struct{})
	rename = func(src, dst string) error {
		err := os.Rename(src, dst)
		if err == nil && src == dir && dst == OldDir(dir) {
			close(paused)
			<-resume
		}
		return err
	}
	defer func() { rename = os.Rename }()

	finished := make(chan error, 1)
	go func() {
		finished <- Install(t.Context(), dir, ownsandv1.GuestOs_GUEST_OS_MACOS)
	}()
	select {
	case <-paused:
	case err := <-finished:
		t.Fatalf("install finished before publication paused: %v", err)
	}
	var model []byte
	read := func() error {
		var err error
		model, err = os.ReadFile(filepath.Join(dir, MacOSHardwareModel))
		return err
	}
	probe := make(chan error, 1)
	go func() { probe <- TryWithBaseImage(dir, read) }()
	select {
	case err := <-probe:
		if !errors.Is(err, ErrLocked) || model != nil {
			t.Errorf("probe between publication renames = %v, model = %q", err, model)
		}
	case <-time.After(5 * time.Second):
		t.Error("probe waited for the publisher")
		close(resume)
		<-finished
		<-probe
		return
	}
	close(resume)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := TryWithBaseImage(dir, read); err != nil || string(model) != "replacement" {
		t.Fatalf("probe after publication = %v, model = %q", err, model)
	}
}

func TestTryWithBaseImageRecoversMacOSBackup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), MacOSDir)
	writeCapabilityImage(t, OldDir(dir), "previous")
	build, err := TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	var model []byte
	read := func() error {
		var err error
		model, err = os.ReadFile(filepath.Join(dir, MacOSHardwareModel))
		return err
	}
	if err := TryWithBaseImage(dir, read); !errors.Is(err, ErrLocked) || model != nil {
		t.Errorf("probe while recovery needs a busy build lock = %v, model = %q", err, model)
	}
	build.releaseAndLog()
	if err := TryWithBaseImage(dir, read); err != nil || string(model) != "previous" {
		t.Fatalf("probe after interrupted install = %v, model = %q", err, model)
	}
	if err := os.Remove(filepath.Join(dir, MacOSHardwareModel)); err != nil {
		t.Fatal(err)
	}
	if err := TryWithBaseImage(dir, read); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe of incomplete image with no backup = %v", err)
	}
}

func writeCapabilityImage(t *testing.T, dir, model string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteMetadata(dir, Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{MacOSDisk, MacOSAuxStorage, MacOSHardwareModel} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(model), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
