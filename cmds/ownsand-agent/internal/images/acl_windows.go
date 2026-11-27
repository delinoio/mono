// SPDX-License-Identifier: FSL-1.1-ALv2

package images

import (
	"path/filepath"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmacl"
)

// RemoveWindowsVMAccess clears VM worker entries before a build is installed
// or a Windows base image is used. The caller must hold the relevant image lock.
func RemoveWindowsVMAccess(dir string) error {
	for _, path := range []string{dir, filepath.Join(dir, WindowsDisk), filepath.Join(dir, MetadataFile)} {
		if err := vmacl.RemoveWorkers(path); err != nil {
			return err
		}
	}
	return nil
}
