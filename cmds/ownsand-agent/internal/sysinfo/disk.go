// SPDX-License-Identifier: FSL-1.1-ALv2

package sysinfo

// availBytes converts the statfs available block count of an unprivileged
// user into bytes.
//
// The unit of the block counts (f_bavail, f_bfree and f_blocks) differs by
// platform: Linux reports them in units of the fragment size (f_frsize), which
// can be smaller than the preferred I/O block size (f_bsize), for example 4096
// against 131072 on some network and FUSE file systems; macOS reports them in
// units of f_bsize. A caller passes the fragment size when the platform has
// one (zero when it does not) and the block size, and a fragment size above
// zero wins over the block size.
func availBytes(bavail uint64, bsize, frsize int64) int64 {
	unit := bsize
	if frsize > 0 {
		unit = frsize
	}
	return int64(bavail) * unit
}
