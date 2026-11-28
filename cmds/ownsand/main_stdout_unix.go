// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand/internal/cli"
)

func runCLI(ctx context.Context, args []string) int {
	env := cli.Env{Args: args}
	// Go can terminate the process with SIGPIPE on a write to descriptor 1.
	// A duplicate above the standard descriptors lets cli.Run receive EPIPE.
	fd, err := unix.FcntlInt(os.Stdout.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		// Defer the error so commands that only write to stderr can still run.
		env.Stdout = stdoutError{err: fmt.Errorf("duplicate stdout: %w", err)}
	} else {
		stdout := os.NewFile(uintptr(fd), os.Stdout.Name())
		defer stdout.Close()
		env.Stdout = stdout
	}
	return cli.Run(ctx, env)
}

type stdoutError struct {
	err error
}

func (w stdoutError) Write([]byte) (int, error) {
	return 0, w.err
}
