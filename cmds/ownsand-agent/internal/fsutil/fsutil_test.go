// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeAndTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("hello"), 0o640)
	os.Symlink("b/f", filepath.Join(src, "a", "link"))
	dst := filepath.Join(t.TempDir(), "copy")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "link")); string(b) != "hello" {
		t.Fatalf("copy: %q", b)
	}
	if n, _ := TreeSize(dst); n != 5 {
		t.Fatalf("size %d", n)
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, src); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := ReadTar(&buf, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a", "b", "f")); string(b) != "hello" {
		t.Fatalf("tar: %q", b)
	}
}

func TestReadTarRejectsEscapes(t *testing.T) {
	for _, hdr := range []*tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../x"},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(hdr)
		tw.Close()
		if err := ReadTar(&buf, t.TempDir()); err == nil {
			t.Errorf("accepted %+v", hdr)
		}
	}
}
