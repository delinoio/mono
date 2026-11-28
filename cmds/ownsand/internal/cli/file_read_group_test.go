// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestFileReadKeepsDestinationGroup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := filepath.Join(dir, "out")
	if err := os.WriteFile(to, []byte("previous contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	creationGID := int(info.Sys().(*syscall.Stat_t).Gid)
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	groups = append(groups, os.Getegid())
	index := slices.IndexFunc(groups, func(gid int) bool { return gid != creationGID })
	if index == -1 {
		t.Skip("requires a group other than the group assigned to newly created files")
	}
	gid := groups[index]
	if err := os.Chown(to, -1, gid); err != nil {
		t.Fatalf("set destination group: %v", err)
	}
	if err := os.Chmod(to, 0o640); err != nil {
		t.Fatalf("set destination mode: %v", err)
	}

	r := newRunner(t)
	r.f.files["/tmp/x"] = []byte("downloaded contents")
	if res := r.run("file", "read", "--to", to, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	info, err = os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(info.Sys().(*syscall.Stat_t).Gid); got != gid {
		t.Errorf("destination group = %d, want %d", got, gid)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("destination mode = %04o, want 0640", got)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "downloaded contents" {
		t.Errorf("destination contents = %q, %v", data, err)
	}
	if entries := mustReadDir(t, dir); !slices.Equal(entries, []string{"out"}) {
		t.Errorf("unexpected destination directory entries: %v", entries)
	}
}
