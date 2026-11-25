// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package snapxfer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestImportRestoresOwnerAccessUnderUmask(t *testing.T) {
	b := newBucket(t)
	good := importModeArchive(t, false)
	bad := importModeArchive(t, true)
	for _, mask := range []int{0o022, 0o077, 0o200, 0o277, 0o177} {
		t.Run(fmt.Sprintf("umask%04o", mask), func(t *testing.T) {
			base := t.TempDir()
			if err := os.Chmod(base, 0o711); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(base, "new-snapshot")
			content := filepath.Join(parent, "content")
			t.Cleanup(func() { removeTestTree(t, parent) })
			tr := &Transfer{HTTP: b.srv.Client(), URLs: b, Logger: quietLogger(), TempDir: t.TempDir()}
			run := func(ctx context.Context, archive []byte) error {
				b.mu.Lock()
				b.objects["/snap/1"] = archive
				b.mu.Unlock()
				sum := sha256.Sum256(archive)
				parts := []*ownsandv1.SnapshotPart{{PartNumber: 1, SizeBytes: int64(len(archive)), Sha256: sum[:]}}
				// These tests run serially. Restore the process umask before
				// any test setup or cleanup.
				old := syscall.Umask(mask)
				defer syscall.Umask(old)
				return tr.Import(ctx, "snap", parts, content)
			}
			requireNoTransferFiles := func() {
				t.Helper()
				if _, err := os.Lstat(content + ".importing"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging after import: %v", err)
				}
				if names := listDir(t, tr.TempDir); len(names) != 0 {
					t.Fatalf("temporary archives after import: %v", names)
				}
			}
			requireNoContent := func() {
				t.Helper()
				requireNoTransferFiles()
				if _, err := os.Lstat(content); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("content after unsuccessful import: %v", err)
				}
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tr.afterExtract = cancel
			if err := run(ctx, good); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel after extraction: %v", err)
			}
			requireNoContent()
			tr.afterExtract = nil
			if err := run(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "unsupported tar entry type") {
				t.Fatalf("extraction failure: %v", err)
			}
			requireNoContent()
			if err := os.Remove(parent); err != nil {
				t.Fatalf("remove empty snapshot parent: %v", err)
			}
			if err := run(t.Context(), good); err != nil {
				t.Fatalf("import with missing snapshot parent: %v", err)
			}
			requireNoTransferFiles()
			requireMode(t, base, 0o711)
			requireMode(t, parent, 0o700)
			requireMode(t, filepath.Join(content, "explicit"), 0o555)
			requireMode(t, filepath.Join(content, "implicit"), 0o700)
			for _, file := range []struct {
				name string
				data string
				mode os.FileMode
			}{{"explicit/file", "payload", 0o444}, {"implicit/file", "second", 0o640}} {
				path := filepath.Join(content, file.name)
				requireMode(t, path, file.mode)
				if got, err := os.ReadFile(path); err != nil || string(got) != file.data {
					t.Errorf("%s: read %q, %v, want %q", file.name, got, err, file.data)
				}
			}
		})
	}
}

func importModeArchive(t *testing.T, invalid bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range []struct {
		hdr  tar.Header
		data string
	}{
		{hdr: tar.Header{Name: "explicit/", Typeflag: tar.TypeDir, Mode: 0o555}},
		{hdr: tar.Header{Name: "explicit/file", Typeflag: tar.TypeReg, Mode: 0o444, Size: 7}, data: "payload"},
		{hdr: tar.Header{Name: "implicit/file", Typeflag: tar.TypeReg, Mode: 0o640, Size: 6}, data: "second"},
	} {
		if err := tw.WriteHeader(&entry.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if invalid {
		if err := tw.WriteHeader(&tar.Header{Name: "unsupported", Typeflag: tar.TypeFifo}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
