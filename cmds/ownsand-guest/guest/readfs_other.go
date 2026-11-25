// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package guest

import "os"

func allowsSnapshotSizeMismatch(*os.File, int64) bool {
	return false
}
