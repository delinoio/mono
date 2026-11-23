// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/snapxfer"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func commandKind(cmd *ownsandv1.AgentCommand) string {
	switch cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CreateSandbox:
		return "create_sandbox"
	case *ownsandv1.AgentCommand_DeleteSandbox:
		return "delete_sandbox"
	case *ownsandv1.AgentCommand_PauseSandbox:
		return "pause_sandbox"
	case *ownsandv1.AgentCommand_ResumeSandbox:
		return "resume_sandbox"
	case *ownsandv1.AgentCommand_CreateSnapshot:
		return "create_snapshot"
	case *ownsandv1.AgentCommand_ExportSnapshot:
		return "export_snapshot"
	case *ownsandv1.AgentCommand_ImportSnapshot:
		return "import_snapshot"
	case *ownsandv1.AgentCommand_CleanUp:
		return "clean_up"
	case *ownsandv1.AgentCommand_UpdateAgent:
		return "update_agent"
	case *ownsandv1.AgentCommand_DeleteSnapshot:
		return "delete_snapshot"
	case *ownsandv1.AgentCommand_WithdrawAgentUpdate:
		return "withdraw_agent_update"
	}
	return "unknown"
}

// isUpdateCommand reports whether cmd takes an update order.
func isUpdateCommand(cmd *ownsandv1.AgentCommand) bool {
	switch cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_UpdateAgent, *ownsandv1.AgentCommand_WithdrawAgentUpdate:
		return true
	}
	return false
}

// updateCommandError maps the updater's refusals to error codes: a command
// that arrives once the installation started or the running version is
// revoked fails its precondition, and a superseded command is aborted.
func updateCommandError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, update.ErrInstalling), errors.Is(err, update.ErrRunningRevoked):
		return &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, Message: err.Error()}
	case errors.Is(err, update.ErrSuperseded):
		return &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_ABORTED, Message: err.Error()}
	}
	return err
}

func errorDetail(err error) *ownsandv1.ErrorDetail {
	var ce *sandboxes.CommandError
	if errors.As(err, &ce) {
		return ce.Detail()
	}
	return &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, Message: err.Error()}
}

// handleCommand runs one command and sends its result on the channel that is
// current when the command ends. A result that cannot be sent is lost; the
// control plane then learns the state from the next state report.
//
// chanCtx ends with the channel connection that delivered the command; agentCtx
// ends only when the agent stops, installs an update or removes the device.
// Commands that change a sandbox belong to the device and run on agentCtx, so
// that a channel loss does not cancel them (a canceled create would record the
// sandbox as failed). Snapshot creation stays on chanCtx: a pending capture is
// not in the state report, so the control plane marks it FAILED on reconnect,
// and no update message reports a late completion. Snapshot transfers and
// agent updates are bound to one channel in the control plane and end with
// chanCtx. Remove this limit when pending snapshots are reported and
// completion has its own update path.
//
// order is the update order that dispatch took when it received an update or
// withdrawal command; it is zero for every other command.
func (a *Agent) handleCommand(chanCtx, agentCtx context.Context, cmd *ownsandv1.AgentCommand, order uint64) {
	kind := commandKind(cmd)
	logger := a.logger.With("command_id", cmd.GetCommandId(), "command", kind)
	logger.Info("command received")
	res := &ownsandv1.AgentCommandResult{CommandId: cmd.GetCommandId()}
	var err error
	switch c := cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CreateSandbox:
		var e *ownsandv1.SandboxStateEntry
		e, err = a.Sandboxes.Create(agentCtx, c.CreateSandbox)
		if e != nil {
			res.Result = &ownsandv1.AgentCommandResult_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxResult{Sandbox: e}}
		}
	case *ownsandv1.AgentCommand_DeleteSandbox:
		var e *ownsandv1.SandboxStateEntry
		e, err = a.Sandboxes.Delete(agentCtx, c.DeleteSandbox)
		setSandboxResult(res, e)
	case *ownsandv1.AgentCommand_PauseSandbox:
		var e *ownsandv1.SandboxStateEntry
		e, err = a.Sandboxes.Pause(agentCtx, c.PauseSandbox)
		setSandboxResult(res, e)
	case *ownsandv1.AgentCommand_ResumeSandbox:
		var e *ownsandv1.SandboxStateEntry
		e, err = a.Sandboxes.Resume(agentCtx, c.ResumeSandbox)
		setSandboxResult(res, e)
	case *ownsandv1.AgentCommand_CreateSnapshot:
		var e *ownsandv1.SnapshotStateEntry
		e, err = a.Sandboxes.CreateSnapshot(chanCtx, c.CreateSnapshot)
		if e != nil {
			res.Result = &ownsandv1.AgentCommandResult_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotResult{Snapshot: e}}
		}
	case *ownsandv1.AgentCommand_ExportSnapshot:
		var r *ownsandv1.ExportSnapshotResult
		r, err = a.exportSnapshot(chanCtx, c.ExportSnapshot)
		if r != nil {
			res.Result = &ownsandv1.AgentCommandResult_ExportSnapshot{ExportSnapshot: r}
		}
	case *ownsandv1.AgentCommand_ImportSnapshot:
		var e *ownsandv1.SnapshotStateEntry
		e, err = a.importSnapshot(chanCtx, c.ImportSnapshot)
		if e != nil {
			res.Result = &ownsandv1.AgentCommandResult_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotResult{Snapshot: e}}
		}
	case *ownsandv1.AgentCommand_CleanUp:
		logger.Info("clean-up command: removing the device")
		a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
		select {
		case a.removal <- struct{}{}:
		default:
		}
		return
	case *ownsandv1.AgentCommand_UpdateAgent:
		err = a.updater.HandleOrdered(chanCtx, c.UpdateAgent, order)
		err = updateCommandError(err)
	case *ownsandv1.AgentCommand_WithdrawAgentUpdate:
		err = updateCommandError(a.updater.WithdrawOrdered(order))
	case *ownsandv1.AgentCommand_DeleteSnapshot:
		err = a.Sandboxes.DeleteSnapshot(c.DeleteSnapshot)
	default:
		err = &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "unknown command"}
	}
	if err != nil {
		res.Error = errorDetail(err)
		logger.Warn("command failed", "code", res.Error.GetCode().String(), "error", res.Error.GetMessage())
	} else {
		logger.Info("command succeeded")
	}
	if !a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}}) {
		// The control plane recovers the outcome from the next state report or
		// sandbox update.
		logger.Warn("command result not sent")
	}
}

func setSandboxResult(res *ownsandv1.AgentCommandResult, e *ownsandv1.SandboxStateEntry) {
	if e != nil {
		res.Result = &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: e}}
	}
}

// beginTransfer reserves the device for one snapshot export or import. Once
// the update installation started it fails with UNAVAILABLE, so the caller
// retries after the agent restarted.
func (a *Agent) beginTransfer() (func(), error) {
	end, err := a.updater.BeginTransfer()
	if err != nil {
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: err.Error()}
	}
	return end, nil
}

func (a *Agent) exportSnapshot(ctx context.Context, cmd *ownsandv1.ExportSnapshotCommand) (*ownsandv1.ExportSnapshotResult, error) {
	dir, release, ok := a.Sandboxes.AcquireSnapshot(cmd.GetSnapshotId())
	if !ok {
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "snapshot is not ready on this device"}
	}
	defer release()
	end, err := a.beginTransfer()
	if err != nil {
		a.logger.Warn("snapshot export refused", "snapshot_id", cmd.GetSnapshotId(), "error", err)
		return nil, err
	}
	defer end()
	parts, size, err := a.transfer.Export(ctx, cmd.GetSnapshotId(), dir, cmd.GetPartSizeBytes())
	if err != nil {
		var tooMany *snapxfer.TooManyPartsError
		if errors.As(err, &tooMany) {
			a.logger.Error("snapshot export failed", "snapshot_id", cmd.GetSnapshotId(), "part_count", tooMany.PartCount, "limit", tooMany.Limit, "error", err)
			return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: fmt.Sprintf("snapshot needs %d parts; the limit is %d", tooMany.PartCount, tooMany.Limit)}
		}
		a.logger.Error("snapshot export failed", "snapshot_id", cmd.GetSnapshotId(), "error", err)
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "snapshot export failed"}
	}
	a.logger.Info("snapshot exported", "snapshot_id", cmd.GetSnapshotId(), "part_count", len(parts), "size_bytes", size)
	return &ownsandv1.ExportSnapshotResult{Parts: parts, SizeBytes: size}, nil
}

// importSnapshot downloads, verifies and publishes a snapshot copy. Imports
// of the same snapshot ID run one at a time (LockSnapshotImport), so that:
//
//   - the ready check, the removal of leftovers, the download, the extraction
//     into the snapshot directory and the publication form one critical
//     section for the ID;
//   - a command that waited for the lock finds the copy that the earlier
//     command published and returns it without a second download;
//   - a failed import removes the snapshot directory only while it holds the
//     lock and before any copy was published, so it never deletes a copy
//     that a ready entry points to.
//
// Imports of different IDs, and every other command, stay concurrent.
func (a *Agent) importSnapshot(ctx context.Context, cmd *ownsandv1.ImportSnapshotCommand) (*ownsandv1.SnapshotStateEntry, error) {
	entry := cmd.GetSnapshot()
	if err := validate.UUIDv7("snapshot_id", entry.GetSnapshotId()); err != nil {
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Message: err.Error()}
	}
	snapshotID := entry.GetSnapshotId()
	unlock, err := a.Sandboxes.LockSnapshotImport(ctx, snapshotID)
	if err != nil {
		a.logger.Warn("snapshot import stopped while it waited for another import of the same snapshot", "snapshot_id", snapshotID, "error", err)
		return nil, err
	}
	defer unlock()
	// Check for a ready copy only after the lock: an import that held the
	// lock before may have published it while this command waited.
	if _, e, ok := a.Sandboxes.SnapshotContent(snapshotID); ok {
		return e, nil
	}
	// Reserve the device against the update installation before the removal
	// of a stale copy, any transfer I/O or any import directory change. The
	// cached-copy answer above needs no reservation: it only reads the entry.
	// The removal below changes the import directory and the manager entry,
	// so an installation that already started must refuse the import before
	// it, and one that is queued waits for the removal and the import.
	end, err := a.beginTransfer()
	if err != nil {
		a.logger.Warn("snapshot import refused", "snapshot_id", snapshotID, "error", err)
		return nil, err
	}
	defer end()
	// A copy that a delete marked, or whose removal failed, is removed before
	// the import writes into its directory.
	if err := a.Sandboxes.PrepareSnapshotImport(snapshotID); err != nil {
		a.logger.Warn("snapshot import refused while an earlier copy is being deleted", "snapshot_id", snapshotID, "error", err)
		return nil, err
	}
	if entry.GetGuestOs() == ownsandv1.GuestOs_GUEST_OS_MACOS {
		// A macOS snapshot boots only on a Mac that supports its hardware
		// model; check it again before the download.
		checker, ok := a.rt.(vmruntime.MacOSHardwareModelChecker)
		if !ok || !checker.MacOSHardwareModelSupported(entry.GetMacosHardwareModel()) {
			return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, Message: "this device does not support the macOS hardware model of the snapshot"}
		}
	}
	var total int64
	for _, p := range cmd.GetParts() {
		total += p.GetSizeBytes()
	}
	if free, err := sysinfo.FreeDiskBytes(a.st.Dir()); err == nil && free < 2*total {
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: "free disk does not fit the snapshot import"}
	}
	dir, err := a.Sandboxes.ImportDir(snapshotID)
	if err != nil {
		return nil, err
	}
	// published becomes true once the ready entry points to the content. Until
	// then the directory holds only this import's partial output, so every
	// failure path removes it (the staging directory lives inside it). The
	// removal runs before unlock, because defers run last in, first out.
	published := false
	defer func() {
		if !published {
			os.RemoveAll(dir)
		}
	}()
	// Remove leftovers of an earlier attempt that a crash interrupted. No
	// ready copy exists here: the check above ran under the lock.
	os.RemoveAll(dir)
	if err := a.transfer.Import(ctx, snapshotID, cmd.GetParts(), filepath.Join(dir, "content")); err != nil {
		a.logger.Error("snapshot import failed", "snapshot_id", snapshotID, "error", err)
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_DATA_LOSS, Message: "snapshot import failed its download or hash check"}
	}
	// A channel that closed while the import renamed the staged content must
	// not publish a ready entry: the deferred removal drops the content.
	if err := ctx.Err(); err != nil {
		a.logger.Warn("snapshot import stopped before publication", "snapshot_id", snapshotID, "error", err)
		return nil, err
	}
	e, err := a.Sandboxes.AddImportedSnapshot(entry)
	if err != nil {
		return nil, err
	}
	published = true
	a.logger.Info("snapshot imported", "snapshot_id", snapshotID)
	return e, nil
}
