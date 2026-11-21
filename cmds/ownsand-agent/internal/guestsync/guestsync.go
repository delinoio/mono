// SPDX-License-Identifier: FSL-1.1-ALv2

// Package guestsync flushes the page cache of a sandbox guest through its
// guest helper, for the pause and snapshot rules of the runtimes.
package guestsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Sync flushes the guest page cache to the disk with `sync` through the
// guest helper of sandboxID, so a disk copy taken while the VM is paused
// holds every completed write. The GuestService has no flush RPC; the exec
// uses an agent-generated exec_id and never reaches a caller.
func Sync(ctx context.Context, sandboxID string, dial guestclient.Dialer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, err := Run(ctx, sandboxID, dial, []string{"sync"})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sync exited with %d", code)
	}
	return nil
}

// Run runs an agent-owned command in the guest through its guest helper and
// returns its exit code. The output is discarded, and the exec never
// reaches a caller. The image builds use it to provision a guest.
func Run(ctx context.Context, sandboxID string, dial guestclient.Dialer, command []string) (int64, error) {
	id, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	client := guestclient.New(dial)
	defer client.Close()
	s := client.Serve(ctx)
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(1, sandboxID, &ownsandv1.ExecRequest{
		SandboxId: sandboxID, ExecId: id, Command: command,
	})}); err != nil {
		return 0, err
	}
	for {
		resp, err := s.Receive()
		if err != nil {
			return 0, err
		}
		ev := resp.GetFrame().GetExecEvent()
		switch {
		case resp.GetFrame().GetError() != nil:
			return 0, errors.New(resp.GetFrame().GetError().GetMessage())
		case ev.GetExited() != nil:
			return exitCode(ev.GetExited())
		case ev.GetFailed() != nil:
			return 0, errors.New(ev.GetFailed().GetMessage())
		case ev.GetSandboxEnded() != nil:
			return 0, errors.New("the guest ended during the command")
		}
	}
}

// exitCode returns the exit code of a command that exited normally. The
// status is a oneof, so GetExitCode alone would read a signaled or unset
// status as a zero exit; those and a timeout are errors.
func exitCode(exited *ownsandv1.ExecExited) (int64, error) {
	if exited.GetTimedOut() {
		return 0, errors.New("the command timed out")
	}
	switch status := exited.GetStatus().(type) {
	case *ownsandv1.ExecExited_ExitCode:
		return status.ExitCode, nil
	case *ownsandv1.ExecExited_Signal:
		return 0, fmt.Errorf("the command was killed by signal %d", status.Signal)
	default:
		return 0, errors.New("the command exited without a status")
	}
}
