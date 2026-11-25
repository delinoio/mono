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
