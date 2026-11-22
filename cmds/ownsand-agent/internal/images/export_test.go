// SPDX-License-Identifier: FSL-1.1-ALv2

package images

// SetRename replaces the rename that Install and Recover use and returns a
// function that restores it.
func SetRename(fn func(oldpath, newpath string) error) (restore func()) {
	prev := rename
	rename = fn
	return func() { rename = prev }
}
