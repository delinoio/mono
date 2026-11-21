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
	id, err := uuidv7.NewString()
	if err != nil {
		return err
	}
	client := guestclient.New(dial)
	defer client.Close()
	s := client.Serve(ctx)
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(1, sandboxID, &ownsandv1.ExecRequest{
		SandboxId: sandboxID, ExecId: id, Command: []string{"sync"},
	})}); err != nil {
		return err
	}
	for {
		resp, err := s.Receive()
		if err != nil {
			return err
		}
		ev := resp.GetFrame().GetExecEvent()
		switch {
		case resp.GetFrame().GetError() != nil:
			return errors.New(resp.GetFrame().GetError().GetMessage())
		case ev.GetExited() != nil:
			return exitedError(ev.GetExited())
		case ev.GetFailed() != nil:
			return errors.New(ev.GetFailed().GetMessage())
		}
	}
}

// exitedError returns nil only when sync exited normally with code 0. The
// status is a oneof, so GetExitCode alone would read a signaled or unset
// status as a zero exit.
func exitedError(exited *ownsandv1.ExecExited) error {
	if exited.GetTimedOut() {
		return errors.New("sync timed out")
	}
	switch status := exited.GetStatus().(type) {
	case *ownsandv1.ExecExited_ExitCode:
		if status.ExitCode != 0 {
			return fmt.Errorf("sync exited with %d", status.ExitCode)
		}
		return nil
	case *ownsandv1.ExecExited_Signal:
		return fmt.Errorf("sync killed by signal %d", status.Signal)
	default:
		return errors.New("sync exited without a status")
	}
}
