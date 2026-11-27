// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
)

const emptyDiskInventory = "<plist><dict><key>images</key><array/></dict></plist>"

func TestMacOSStaleBuildRemoval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mountPath string
		mountErr  error
		inventory func(build string) string
		infoErr   error
		wantErr   string
	}{
		{name: "active Data mount", mountPath: "mnt", wantErr: "is still active"},
		{name: "mount at staging root", mountPath: ".", wantErr: "is still active"},
		{name: "unavailable mount table", mountErr: errors.New("mount table unavailable"), wantErr: "mount table unavailable"},
		{name: "inconclusive mount point", mountPath: "vanished", wantErr: "verify host mount point"},
		{name: "attached without a mount", inventory: func(build string) string {
			return diskInfoPlist(filepath.Join(build, images.MacOSDisk), "/dev/disk4")
		}, wantErr: "is still attached"},
		{name: "unavailable attachments", infoErr: errors.New("hdiutil unavailable"), wantErr: "hdiutil unavailable"},
		{name: "truncated inventory", inventory: func(string) string {
			return strings.TrimSuffix(emptyDiskInventory, "</plist>")
		}, wantErr: "attached disk inventory"},
		{name: "inventory without images array", inventory: func(string) string {
			return "<plist><dict><key>images</key><dict/></dict></plist>"
		}, wantErr: "no images array"},
		{name: "inventory without image path", inventory: func(string) string {
			return diskInfoPlist("", "/dev/disk4")
		}, wantErr: "invalid image path"},
		{name: "detached with an unrelated mounted image", mountPath: "../macos.build-other/mnt", inventory: func(build string) string {
			return diskInfoPlist(filepath.Join(build+"-other", images.MacOSDisk), "/dev/disk4")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := filepath.Join(t.TempDir(), "macos.build")
			sentinel := filepath.Join(build, "mnt", "sentinel")
			if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(build+"-other", "mnt"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{sentinel, filepath.Join(build, images.MacOSDisk), filepath.Join(build+"-other", images.MacOSDisk)} {
				if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mounts := func() ([]string, error) {
				if tc.mountPath == "" {
					return nil, tc.mountErr
				}
				return []string{filepath.Join(build, tc.mountPath)}, nil
			}
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name != hdiutilPath || strings.Join(args, " ") != "info -plist" {
					t.Fatalf("unexpected host tool: %s %v", name, args)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("attachment check has no deadline")
				}
				if tc.inventory != nil {
					return []byte(tc.inventory(build)), tc.infoErr
				}
				return []byte(emptyDiskInventory), tc.infoErr
			}
			err := removeMacOSBuild(t.Context(), run, mounts, build)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), build) {
					t.Fatalf("cleanup error = %v, want staging path and %q", err, tc.wantErr)
				}
				if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
					t.Fatalf("staged sentinel = %q, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(build); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging stat error = %v, want os.ErrNotExist", err)
			}
		})
	}
}
