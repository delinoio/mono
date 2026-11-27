// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package fsutil

import (
	"context"
	"os"
)

func repairTarDirectoryLinks(context.Context, *os.Root, []string) (bool, error) {
	return false, nil
}
