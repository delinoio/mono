// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package cli

import (
	"context"
	"io"
	"os"
)

func uploadFileReader(_ context.Context, f *os.File) (io.Reader, error) {
	return f, nil
}
