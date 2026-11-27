// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package cli

import (
	"context"
	"io"
	"os"
)

func downloadFileWriter(_ context.Context, f *os.File) (io.Writer, error) {
	return f, nil
}
