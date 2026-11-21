// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package guest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// symlinkFixture builds
//
//	<root>/front/file            sentinel "front"
//	<root>/other/file            sentinel "other"
//	<root>/other/nested/
//	<root>/front/link  ->  <root>/other/nested
//
// so that "<root>/front/link/../file" names <root>/other/file by the
// operating system but <root>/front/file after a lexical clean.
func symlinkFixture(t *testing.T) (root, viaLink string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"front", filepath.Join("other", "nested")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"front", "other"} {
		if err := os.WriteFile(filepath.Join(root, name, "file"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "other", "nested"), filepath.Join(root, "front", "link")); err != nil {
		t.Fatal(err)
	}
	// Built by concatenation: filepath.Join would clean the "..".
	return root, root + "/front/link/../file"
}

func TestReadFileResolvesDotDotAfterSymlink(t *testing.T) {
	h := newHarness(t, nil)
	root, viaLink := symlinkFixture(t)
	if got, want := filepath.Clean(viaLink), filepath.Join(root, "front", "file"); got != want {
		t.Fatalf("fixture: clean path %q, want %q", got, want)
	}
	req := readReq(t, viaLink)
	data, size, errd := h.readAll(req)
	if errd != nil || string(data) != "other" || size != 5 {
		t.Fatalf("read via symlink: %q size %d err %v", data, size, errd)
	}
	// A resumed read of the same read_id with the exact same path serves the
	// snapshot suffix, also after the original changed.
	if err := os.WriteFile(filepath.Join(root, "other", "file"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	req.Offset = 2
	data, size, errd = h.readAll(req)
	if errd != nil || string(data) != "her" || size != 5 {
		t.Fatalf("resumed read: %q size %d err %v", data, size, errd)
	}
	// The cleaned spelling is another path: the read_id binding compares the
	// exact string.
	req.Path = filepath.Clean(viaLink)
	req.Offset = 0
	if _, _, errd := h.readAll(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS {
		t.Fatalf("cleaned path on the same read_id: %v", errd)
	}
}

func TestWriteFileResolvesDotDotAfterSymlink(t *testing.T) {
	h := newHarness(t, nil)
	root, viaLink := symlinkFixture(t)
	id := newID(t)
	first := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Path: viaLink, BindingSalt: make([]byte, 32), Offset: 0, Data: []byte("new")}
	if _, errd := h.write(first); errd != nil {
		t.Fatalf("first chunk: %v", errd)
	}
	tmpName := "." + "file" + ".ownsand-" + id + ".tmp"
	if _, err := os.Stat(filepath.Join(root, "other", tmpName)); err != nil {
		t.Fatalf("temporary file is not in the native target directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "front", tmpName)); !os.IsNotExist(err) {
		t.Fatalf("temporary file in the lexical directory: %v", err)
	}
	// A repeated first chunk resumes; a chunk without a path is allowed; the
	// cleaned spelling is bound to another path.
	if _, errd := h.write(first); errd != nil {
		t.Fatalf("repeated first chunk: %v", errd)
	}
	other := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Path: filepath.Clean(viaLink), Offset: 3, Data: []byte("x")}
	if _, errd := h.write(other); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("cleaned path on the same write_id: %v", errd)
	}
	fin := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Offset: 3, Data: []byte("er"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 5}}
	if resp, errd := h.write(fin); errd != nil || !resp.GetFinished() {
		t.Fatalf("finish without path: %v %v", resp, errd)
	}
	for name, want := range map[string]string{
		filepath.Join(root, "other", "file"): "newer",
		filepath.Join(root, "front", "file"): "front",
	} {
		if got, err := os.ReadFile(name); err != nil || string(got) != want {
			t.Fatalf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	for _, dir := range []string{"front", "other"} {
		if _, err := os.Stat(filepath.Join(root, dir, tmpName)); !os.IsNotExist(err) {
			t.Fatalf("temporary file left in %s: %v", dir, err)
		}
	}
	// A repeated identical finish returns the stored result.
	if resp, errd := h.write(fin); errd != nil || !resp.GetFinished() || resp.GetSizeBytes() != 5 {
		t.Fatalf("repeated finish: %v %v", resp, errd)
	}
	h.clock.Advance(16 * time.Minute)
	h.srv.Sweep()
}

func TestWriteFileTargetForms(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	write := func(path string) *ownsandv1.ErrorDetail {
		_, errd := h.write(&ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: path, BindingSalt: make([]byte, 32), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 0}})
		return errd
	}
	// A trailing separator is not part of the file name.
	if errd := write(dir + "/nodir/trailing/"); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("missing parent with trailing separator: %v", errd)
	}
	if errd := write(dir + "/file//"); errd != nil {
		t.Fatalf("trailing separators: %v", errd)
	}
	if st, err := os.Stat(filepath.Join(dir, "file")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("file with trailing separators: %v", err)
	}
	for _, path := range []string{dir + "/.", dir + "/..", dir + "/", "/"} {
		if errd := write(path); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("directory target %q: %v", path, errd)
		}
	}
	// Doubled separators and "." elements in the directory part resolve
	// natively.
	if errd := write(dir + "//./sub/../doubled"); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("missing sub directory: %v", errd)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if errd := write(dir + "//./sub/../doubled"); errd != nil {
		t.Fatalf("doubled separators: %v", errd)
	}
	if _, err := os.Stat(filepath.Join(dir, "doubled")); err != nil {
		t.Fatal(err)
	}
}

func TestSplitTargetPOSIX(t *testing.T) {
	for _, c := range []struct{ in, dir, base string }{
		{"/a/b", "/a/", "b"},
		{"/file", "/", "file"},
		{"/a/link/../file", "/a/link/../", "file"},
		{"/a/b//", "/a/", "b"},
		{"/a/..", "/a/", ".."},
		{"/", "", ""},
	} {
		if dir, base := splitTarget(c.in); dir != c.dir || base != c.base {
			t.Errorf("splitTarget(%q) = %q, %q; want %q, %q", c.in, dir, base, c.dir, c.base)
		}
	}
}
