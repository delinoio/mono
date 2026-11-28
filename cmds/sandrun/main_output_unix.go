// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/sandrun/internal/cli"
)

func runCLI(ctx context.Context, env cli.Env) int {
	stderr, err := duplicateCLIOutput(os.Stderr)
	if err != nil {
		// Write directly because os.Stderr.Write can raise SIGPIPE before
		// returning an error on a closed pipe.
		_, _ = unix.Write(unix.Stderr, fmt.Appendf(nil, "error: %v\n", err))
		return cli.ExitError
	}
	defer stderr.Close()

	stdout, err := duplicateCLIOutput(os.Stdout)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return cli.ExitError
	}
	defer stdout.Close()

	env.Stdout = stdout
	env.Stderr = stderr
	return cli.Run(ctx, env)
}

func duplicateCLIOutput(f *os.File) (*os.File, error) {
	// Go returns EPIPE from writes to descriptors above 2. Writes using
	// os.Stdout or os.Stderr can terminate the process with SIGPIPE. Keep the
	// inherited descriptors available for the MCP SDK's stdio transport.
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("duplicate %s: %w", f.Name(), err)
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}
