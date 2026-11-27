// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRepairTarDirectoryLinksWithoutSymlinkPrivilege(t *testing.T) {
	createErr := errors.New("link creation failed")
	for _, tc := range []struct {
		name      string
		cancel    bool
		createErr error
		wantDir   bool
	}{
		{name: "repair", wantDir: true},
		{name: "already a directory link", wantDir: true},
		{name: "canceled before replacement", cancel: true},
		{name: "link creation fails", createErr: createErr},
		{name: "creation still lacks the directory attribute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const name = "entry"
			target := filepath.FromSlash("../target/dir")
			alreadyDirectory := tc.name == "already a directory link"
			// Use ordinary entries for the Lstat attributes. Simulate only
			// Readlink and Symlink, which need a real Windows symbolic link.
			if alreadyDirectory {
				err = root.Mkdir(name, 0o700)
			} else {
				err = root.WriteFile(name, nil, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var replacementCalls int
			links := &tarDirectoryLinkTestRoot{
				Root: root,
				readlink: func(name string) (string, error) {
					if tc.cancel {
						cancel()
					}
					return target, nil
				},
				symlink: func(gotTarget, gotName string) error {
					replacementCalls++
					if gotTarget != target || gotName != name {
						t.Errorf("replacement = (%q, %q); want (%q, %q)", gotTarget, gotName, target, name)
					}
					if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("old entry remains before replacement: %v", err)
					}
					if tc.createErr != nil {
						return tc.createErr
					}
					if tc.wantDir {
						return root.Mkdir(name, 0o700)
					}
					return root.WriteFile(name, nil, 0o600)
				},
			}
			repaired, err := repairTarDirectoryLinks(ctx, links, []string{name})
			if want := tc.wantDir && !alreadyDirectory; repaired != want {
				t.Errorf("repaired = %t; want %t", repaired, want)
			}
			switch {
			case tc.cancel:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("repair error = %v; want context.Canceled", err)
				}
				if _, err := root.Lstat(name); err != nil {
					t.Errorf("cancellation removed the original entry: %v", err)
				}
			case tc.createErr != nil:
				if !errors.Is(err, tc.createErr) {
					t.Fatalf("repair error = %v; want %v", err, tc.createErr)
				}
			case tc.wantDir:
				if err != nil {
					t.Fatal(err)
				}
			default:
				if err == nil {
					t.Fatal("accepted a replacement without the directory attribute")
				}
			}
			wantCalls := 1
			if tc.cancel || alreadyDirectory {
				wantCalls = 0
			}
			if replacementCalls != wantCalls {
				t.Errorf("replacement calls = %d; want %d", replacementCalls, wantCalls)
			}
		})
	}
}

type tarDirectoryLinkTestRoot struct {
	*os.Root
	readlink func(string) (string, error)
	symlink  func(string, string) error
}

func (r *tarDirectoryLinkTestRoot) Readlink(name string) (string, error) {
	return r.readlink(name)
}

func (r *tarDirectoryLinkTestRoot) Symlink(target, name string) error {
	return r.symlink(target, name)
}
