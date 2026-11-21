// SPDX-License-Identifier: Apache-2.0

package fcrt

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

// syncGuest flushes the guest page cache to the disk with `sync` through
// the guest helper, so a disk copy taken while the VM is paused holds every
// completed write. The GuestService has no flush RPC; the exec uses an
// agent-generated exec_id and never reaches a caller.
func syncGuest(ctx context.Context, v *vm) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := uuidv7.NewString()
	if err != nil {
		return err
	}
	client := guestclient.New(v.Dial)
	defer client.Close()
	s := client.Serve(ctx)
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(1, v.id, &ownsandv1.ExecRequest{
		SandboxId: v.id, ExecId: id, Command: []string{"sync"},
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
			if ev.GetExited().GetExitCode() != 0 {
				return fmt.Errorf("sync exited with %d", ev.GetExited().GetExitCode())
			}
			return nil
		case ev.GetFailed() != nil:
			return errors.New(ev.GetFailed().GetMessage())
		}
	}
}
