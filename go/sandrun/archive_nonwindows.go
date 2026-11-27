// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package sandrun

import (
	"context"
	"os"
)

func finishArchiveLinks(ctx context.Context, root *os.Root, _ *extractStats) error {
	return verifyLinks(ctx, root.Name(), nil)
}
