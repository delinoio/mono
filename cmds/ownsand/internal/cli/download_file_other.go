// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package cli

import (
	"context"
	"io"
	"os"
)

func prepareDownloadReplacement(f *os.File, destination os.FileInfo) error {
	return f.Chmod(destination.Mode().Perm())
}

func downloadFileWriter(_ context.Context, f *os.File) (io.Writer, error) {
	return f, nil
}
