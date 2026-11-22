// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// TestSendCommandEndedContextQueuesNothing checks that sendCommand fails
// with the caller's context error, queues no command and keeps no record of
// it when the context already ended, although the outbound queue has room.
// The CreateSnapshot and ExportSnapshot handlers move their committed
// record to a failed state on exactly this error.
func TestSendCommandEndedContextQueuesNothing(t *testing.T) {
	s := &Service{Now: time.Now}
	conn := hub.New().Register(context.Background(), "dev", "org")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdCreateSnapshot, orgID: "org", snapshotID: "snap"},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: "snap", SandboxId: "sb"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sendCommand = %v, want context.Canceled", err)
	}
	select {
	case env := <-conn.Outbound():
		t.Fatalf("a command was queued for an ended context: %v", env)
	default:
	}
	if n := s.commands.len(); n != 0 {
		t.Fatalf("pending command records = %d, want 0", n)
	}
}

// TestFailCreateSendNotQueuedKeepsClosedChannelToReconciliation checks that
// a send error on a channel that is already closed fails nothing: the
// reconnect reconciliation owns the sandbox then. The service has no
// database, so any attempt to fail the create would panic.
func TestFailCreateSendNotQueuedKeepsClosedChannelToReconciliation(t *testing.T) {
	s := &Service{Now: time.Now, Logger: slog.New(slog.DiscardHandler)}
	conn := hub.New().Register(context.Background(), "dev", "org")
	conn.Close(hub.ErrQueueFull)
	if got := s.failCreateSendNotQueued(context.Background(), conn, commandInfo{kind: cmdCreateSandbox, orgID: "org", sandboxID: "sb"}, hub.ErrQueueFull); got != nil {
		t.Fatalf("failCreateSendNotQueued on a closed channel = %v, want nil", got)
	}
}
