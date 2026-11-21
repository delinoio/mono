// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// canonicalUUID returns the lowercase canonical text of a valid UUID v7 and
// any other value unchanged. Agents may spell an identifier in either hex
// case, but the stored identifiers are lowercase uuid::text, so report maps
// and revision epochs must compare by value, not by spelling. A value that
// is not a UUID v7 stays as it is and never equals a stored identifier.
func canonicalUUID(s string) string {
	id, err := uuidv7.Parse(s)
	if err != nil {
		return s
	}
	return id.String()
}

// compareRevision orders sandbox revisions: a newer epoch (a later UUID v7,
// which sorts lexicographically by time in lowercase canonical form) always
// wins, and within one epoch the higher counter wins. The empty epoch of a
// server-made state is older than every agent revision. Callers pass epochs
// in lowercase canonical form (see canonicalUUID); the comparison is
// byte-wise, so an uppercase spelling would sort before every lowercase one.
func compareRevision(epochA string, counterA uint64, epochB string, counterB uint64) int {
	switch {
	case epochA < epochB:
		return -1
	case epochA > epochB:
		return 1
	case counterA < counterB:
		return -1
	case counterA > counterB:
		return 1
	}
	return 0
}

// validAgentState reports whether an agent may report state.
func validAgentState(state ownsandv1.SandboxState) bool {
	switch state {
	case ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxState_SANDBOX_STATE_DELETING,
		ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxState_SANDBOX_STATE_FAILED:
		return true
	}
	return false
}

// pendingDelete is a delete command that the server must send after the
// transaction commits.
type pendingDelete struct {
	sandboxID string
	reason    ownsandv1.SandboxEndReason
}

// entryResult is the outcome of applying one agent sandbox entry.
type entryResult struct {
	known   bool
	applied bool
	// delete is set when the agent still runs a VM that must go.
	delete *pendingDelete
}

// applySandboxEntry applies an agent-reported sandbox entry of deviceID
// under the revision rule. Terminal states are absorbing: a later entry
// never changes them, and a live VM of a terminal sandbox gets a delete
// command.
func (s *Service) applySandboxEntry(ctx context.Context, tx pgx.Tx, deviceID string, e *ownsandv1.SandboxStateEntry) (entryResult, error) {
	var res entryResult
	if !uuidv7.Valid(e.GetSandboxId()) || !validAgentState(e.GetState()) || e.GetRevision() == nil || !uuidv7.Valid(e.GetRevision().GetEpoch()) {
		return res, nil
	}
	sandboxID := canonicalUUID(e.GetSandboxId())
	epoch := canonicalUUID(e.GetRevision().GetEpoch())
	r, err := scanSandbox(tx.QueryRow(ctx, sandboxSelect+" WHERE s.sandbox_id = $1 AND s.device_id = $2 FOR UPDATE OF s", sandboxID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		// The agent runs a VM that the server does not know (for example of a
		// deleted record): delete it.
		if !isTerminal(e.GetState()) {
			res.delete = &pendingDelete{sandboxID: sandboxID, reason: ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER}
		}
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.known = true
	if isTerminal(r.AgentState) {
		if !isTerminal(e.GetState()) {
			res.delete = &pendingDelete{sandboxID: r.ID, reason: r.EndReason}
		}
		return res, nil
	}
	rev := e.GetRevision()
	// A stored epoch may still be in the spelling an earlier version wrote, so
	// canonicalize it before the byte-wise comparison.
	if compareRevision(epoch, rev.GetCounter(), canonicalUUID(r.Epoch), r.Counter) > 0 {
		res.applied = true
		var idle any
		if e.GetIdleDeadline() != nil {
			idle = e.GetIdleDeadline().AsTime()
		}
		var restore any
		if e.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_UNSPECIFIED {
			restore = int16(e.GetRestorePath())
		}
		if isTerminal(e.GetState()) {
			reason := e.GetEndReason()
			if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
				switch {
				case r.DeletePending:
					reason = r.DeleteEndReason
				case e.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED:
					reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
				default:
					reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE sandboxes SET revision_epoch = $2, revision_counter = $3,
				idle_deadline = COALESCE($4, idle_deadline), restore_path = COALESCE($5, restore_path) WHERE sandbox_id = $1`,
				r.ID, epoch, int64(rev.GetCounter()), idle, restore); err != nil {
				return res, err
			}
			return res, s.endSandboxTx(ctx, tx, r, e.GetState(), reason, r.deleteActor())
		}
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET agent_state = $2, revision_epoch = $3, revision_counter = $4,
			idle_deadline = COALESCE($5, idle_deadline), restore_path = COALESCE($6, restore_path) WHERE sandbox_id = $1`,
			r.ID, int16(e.GetState()), epoch, int64(rev.GetCounter()), idle, restore); err != nil {
			return res, err
		}
	}
	// An entry with a lower revision is ignored; an equal one confirms the
	// stored state. Either way a pending delete is sent again while the
	// agent lists the sandbox as not deleted.
	if r.DeletePending && !isTerminal(e.GetState()) {
		res.delete = &pendingDelete{sandboxID: r.ID, reason: r.DeleteEndReason}
	}
	return res, nil
}

// reconcileResult is what the channel does after a state report committed.
type reconcileResult struct {
	deletes []pendingDelete
	changed []string
	// snapshotDeletes are listed snapshots without a record: their record
	// was deleted while the device was offline or before it learned the
	// delete-snapshot command.
	snapshotDeletes []string
	// recoveredCopies counts the snapshot_copies rows that the report
	// created for listed copies that the server did not track.
	recoveredCopies int
	// removed is set when the device was already removed (or is gone) when
	// the report took the device row lock. The transaction then changed
	// nothing, and the channel must not acknowledge the report.
	removed bool
}

// reconcileStateReport applies a full state report of the device: each
// listed sandbox under the revision rule, the sandboxes that the report
// omits, the snapshots and the built base images. It marks the device
// online in the same transaction, so the device is eligible only after its
// report was applied.
//
// Lock order: the transaction takes FOR KEY SHARE on the organization row
// first, then locks the device row and keeps the lock to commit, then the
// snapshot and sandbox rows, like removeDeviceTx, which updates the device
// row before it ends the device's sandboxes. A removal that commits first
// makes the report a no-op (reconcileResult.removed); a report that commits
// first is followed by the removal, which ends the sandboxes.
//
// The organization lock serializes the report with an organization
// deletion, which removes the devices of the organization one at a time and
// locks the exported snapshots of each removed device. A report of device B
// that locks a snapshot exported from device A while it holds the row of B
// would otherwise wait for a deletion that already removed A and waits for
// B. FOR KEY SHARE lets reports of one organization run together, and it
// conflicts with the FOR UPDATE of the deletion: a deletion that started
// first makes the report wait before it takes any device or snapshot lock
// (then it finds no organization and changes nothing), and a report that
// started first finishes before the deletion takes its first device lock.
// Enroll uses the same order (see EnrollDevice).
//
// channelID is the ID of the channel that sent the report. An export that a
// retained, listed snapshot still has in EXPORTING and that another channel
// (or no recorded channel) owns lost its owner: the command or its result
// was lost with the earlier channel or control plane process, so the
// report fails the export.
func (s *Service) reconcileStateReport(ctx context.Context, deviceID, channelID string, report *ownsandv1.StateReport) (reconcileResult, error) {
	var out reconcileResult
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		out = reconcileResult{}
		// Step 1 resolves the organization without a lock, so the
		// transaction never holds the device lock while it waits for the
		// organization lock. The organization of a device never changes.
		var orgID string
		err := tx.QueryRow(ctx, "SELECT organization_id::text FROM devices WHERE device_id = $1", deviceID).Scan(&orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			out.removed = true
			return nil
		}
		if err != nil {
			return err
		}
		var locked int
		err = tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR KEY SHARE", orgID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			// The organization was deleted: its devices are removed.
			out.removed = true
			return nil
		}
		if err != nil {
			return err
		}
		var state int16
		var capabilities []byte
		err = tx.QueryRow(ctx, "SELECT state, capabilities FROM devices WHERE device_id = $1 FOR UPDATE", deviceID).Scan(&state, &capabilities)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED) {
			out.removed = true
			return nil
		}
		if err != nil {
			return err
		}
		listed := map[string]bool{}
		for _, e := range report.GetSandboxes() {
			listed[canonicalUUID(e.GetSandboxId())] = true
			res, err := s.applySandboxEntry(ctx, tx, deviceID, e)
			if err != nil {
				return err
			}
			if res.applied {
				// The stored ID, so that the change watchers fire by value.
				out.changed = append(out.changed, canonicalUUID(e.GetSandboxId()))
			}
			if res.delete != nil {
				out.deletes = append(out.deletes, *res.delete)
			}
		}
		rows, err := tx.Query(ctx, sandboxSelect+" WHERE s.device_id = $1 AND NOT (s.agent_state = ANY($2)) FOR UPDATE OF s", deviceID, terminalSandboxStates)
		if err != nil {
			return err
		}
		var missing []*sandboxRow
		for rows.Next() {
			r, err := scanSandbox(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if !listed[r.ID] {
				missing = append(missing, r)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range missing {
			state, reason := ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
			if r.DeletePending || r.AgentState == ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
				state, reason = ownsandv1.SandboxState_SANDBOX_STATE_DELETED, r.DeleteEndReason
				if reason == 0 {
					reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
				}
			}
			if err := s.endSandboxTx(ctx, tx, r, state, reason, r.deleteActor()); err != nil {
				return err
			}
			out.changed = append(out.changed, r.ID)
		}
		recovered, err := s.reconcileSnapshotsTx(ctx, tx, deviceID, channelID, report.GetSnapshots())
		if err != nil {
			return err
		}
		out.recoveredCopies = recovered
		unknown, err := unknownSnapshotsTx(ctx, tx, report.GetSnapshots())
		if err != nil {
			return err
		}
		out.snapshotDeletes = unknown
		if err := s.mergeBuiltImagesTx(ctx, tx, deviceID, capabilities, report.GetBuiltBaseImages()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE devices SET state = $2, state_epoch = $3, last_seen_time = $4 WHERE device_id = $1 AND state <> $5",
			deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE), report.GetStateEpoch(), s.now(), int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
		return err
	})
	return out, err
}

// reconcileSnapshotsTx removes the local copies that the report does not
// list: an original copy of an exported snapshot keeps its record without a
// device, any other snapshot record is deleted with its templates, and an
// imported copy loses its copy row. Listed snapshots that the server still
// has in CREATING take the reported fields, and a listed snapshot whose
// export is EXPORTING under another channel than channelID (or under no
// recorded channel) becomes EXPORT_FAILED with a clean-up job for the
// attempt's objects. It also recovers the copy rows of listed READY copies
// of READY, EXPORTED snapshots that the server does not track (the only
// other writer is the in-memory import result, which a lost channel or a
// restart loses) and returns the number of rows it created.
//
// Lock order: the organization (FOR KEY SHARE) and the device row first, as
// reconcileStateReport takes them (the order of device removal and
// organization deletion), then the snapshot rows in snapshot_id order in
// one statement, so that reports of different devices that share an
// exported snapshot never wait on each other in opposite orders. The
// snapshot row lock conflicts with the one that DeleteSnapshot takes: when
// the report commits first, the deletion reads the recovered row and
// records an event for the device; when the deletion commits first, the
// report finds no record, creates no row and the report's unknown-snapshot
// rule schedules the stale-copy clean-up.
func (s *Service) reconcileSnapshotsTx(ctx context.Context, tx pgx.Tx, deviceID, channelID string, entries []*ownsandv1.SnapshotStateEntry) (int, error) {
	listed := map[string]*ownsandv1.SnapshotStateEntry{}
	var readyIDs []string
	for _, e := range entries {
		snapshotID := canonicalUUID(e.GetSnapshotId())
		listed[snapshotID] = e
		if (e.GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY || e.GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_UNSPECIFIED) &&
			validateID("snapshot_id", e.GetSnapshotId()) == nil {
			readyIDs = append(readyIDs, snapshotID)
		}
	}
	var devOrg string
	var devHost, devArch, devState int16
	if err := tx.QueryRow(ctx, "SELECT organization_id::text, host_os, arch, state FROM devices WHERE device_id = $1 FOR UPDATE", deviceID).Scan(&devOrg, &devHost, &devArch, &devState); err != nil {
		return 0, err
	}
	if ownsandv1.DeviceState(devState) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		// A removed device never gets a copy row.
		readyIDs = nil
	}
	rows, err := tx.Query(ctx, `SELECT snapshot_id::text, organization_id::text, state, export_state,
			COALESCE(export_command_id::text, ''), COALESCE(export_channel_id::text, ''), device_id = $1 FROM snapshots
		WHERE device_id = $1 OR (snapshot_id = ANY($2::uuid[]) AND organization_id = $3 AND state = $4 AND export_state = $5
			AND host_os = $6 AND arch = $7)
		ORDER BY snapshot_id FOR UPDATE`,
		deviceID, readyIDs, devOrg, int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_READY),
		int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED), devHost, devArch)
	if err != nil {
		return 0, err
	}
	type snap struct {
		id, org, command, channel string
		state, export             int16
		owned                     bool
	}
	var snaps []snap
	for rows.Next() {
		var sn snap
		var owned *bool
		if err := rows.Scan(&sn.id, &sn.org, &sn.state, &sn.export, &sn.command, &sn.channel, &owned); err != nil {
			rows.Close()
			return 0, err
		}
		sn.owned = owned != nil && *owned
		snaps = append(snaps, sn)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	recovered := 0
	for _, sn := range snaps {
		if !sn.owned {
			// A candidate for copy recovery: another device holds the original
			// copy or none does.
			tag, err := tx.Exec(ctx, "INSERT INTO snapshot_copies (snapshot_id, device_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", sn.id, deviceID)
			if err != nil {
				return 0, err
			}
			recovered += int(tag.RowsAffected())
			continue
		}
		e, ok := listed[sn.id]
		if ok {
			if ownsandv1.SnapshotState(sn.state) == ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING && e.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
				if err := applySnapshotEntryTx(ctx, tx, sn.id, e); err != nil {
					return 0, err
				}
			}
			if ownsandv1.SnapshotExportState(sn.export) == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING && sn.channel != channelID {
				if err := s.failExportTx(ctx, tx, sn.org, sn.id, sn.command, "owner_channel_replaced"); err != nil {
					return 0, err
				}
			}
			continue
		}
		if ownsandv1.SnapshotExportState(sn.export) == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED {
			if _, err := tx.Exec(ctx, "UPDATE snapshots SET device_id = NULL WHERE snapshot_id = $1", sn.id); err != nil {
				return 0, err
			}
			continue
		}
		if ownsandv1.SnapshotState(sn.state) == ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
			// A snapshot that the agent never reported has failed; its record
			// stays in FAILED so callers see the outcome.
			if _, err := tx.Exec(ctx, "UPDATE snapshots SET state = $2 WHERE snapshot_id = $1", sn.id, int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED)); err != nil {
				return 0, err
			}
			continue
		}
		if err := s.deleteSnapshotRecordTx(ctx, tx, sn.org, sn.id); err != nil {
			return 0, err
		}
	}
	copyRows, err := tx.Query(ctx, "SELECT snapshot_id::text FROM snapshot_copies WHERE device_id = $1", deviceID)
	if err != nil {
		return 0, err
	}
	var copies []string
	for copyRows.Next() {
		var id string
		if err := copyRows.Scan(&id); err != nil {
			copyRows.Close()
			return 0, err
		}
		copies = append(copies, id)
	}
	copyRows.Close()
	if err := copyRows.Err(); err != nil {
		return 0, err
	}
	for _, id := range copies {
		if _, ok := listed[id]; !ok {
			if _, err := tx.Exec(ctx, "DELETE FROM snapshot_copies WHERE snapshot_id = $1 AND device_id = $2", id, deviceID); err != nil {
				return 0, err
			}
		}
	}
	return recovered, nil
}

// unknownSnapshotsTx returns the listed snapshot IDs that have no record.
// The control plane creates the record of every snapshot before it sends
// the create or import command, so a listed snapshot without a record was
// deleted and its local copy is stale.
func unknownSnapshotsTx(ctx context.Context, tx pgx.Tx, entries []*ownsandv1.SnapshotStateEntry) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	var ids []string
	for _, e := range entries {
		if validateID("snapshot_id", e.GetSnapshotId()) == nil {
			ids = append(ids, e.GetSnapshotId())
		}
	}
	rows, err := tx.Query(ctx, "SELECT id FROM unnest($1::uuid[]) AS listed(id) WHERE NOT EXISTS (SELECT 1 FROM snapshots WHERE snapshot_id = listed.id)", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var unknown []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		unknown = append(unknown, id)
	}
	return unknown, rows.Err()
}

// applySnapshotEntryTx stores the agent's report of a snapshot.
func applySnapshotEntryTx(ctx context.Context, tx pgx.Tx, snapshotID string, e *ownsandv1.SnapshotStateEntry) error {
	state := e.GetState()
	if state == ownsandv1.SnapshotState_SNAPSHOT_STATE_UNSPECIFIED {
		state = ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	}
	_, err := tx.Exec(ctx, `UPDATE snapshots SET state = $2, has_memory_state = $3, size_bytes = $4,
		macos_hardware_model = COALESCE($5, macos_hardware_model) WHERE snapshot_id = $1`,
		snapshotID, int16(state), e.GetHasMemoryState(), e.GetSizeBytes(), nilIfEmpty(e.GetMacosHardwareModel()))
	return err
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// mergeBuiltImagesTx stores the built base images of a state report in the
// device capabilities. The caller holds the device row lock and passes the
// stored capabilities that it read under that lock (raw), so the merge does
// not lock the row again.
func (s *Service) mergeBuiltImagesTx(ctx context.Context, tx pgx.Tx, deviceID string, raw []byte, images []ownsandv1.BaseImage) error {
	caps := &ownsandv1.DeviceCapabilities{}
	if len(raw) > 0 {
		if err := proto.Unmarshal(raw, caps); err != nil {
			return err
		}
	} else if len(images) == 0 {
		return nil
	}
	if slices.Equal(caps.GetBuiltBaseImages(), images) {
		return nil
	}
	caps.BuiltBaseImages = images
	out, err := proto.Marshal(caps)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE devices SET capabilities = $2 WHERE device_id = $1", deviceID, out)
	return err
}

// applySandboxUpdate applies one sandbox update in its own transaction.
func (s *Service) applySandboxUpdate(ctx context.Context, deviceID string, e *ownsandv1.SandboxStateEntry) (entryResult, error) {
	var res entryResult
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = s.applySandboxEntry(ctx, tx, deviceID, e)
		return err
	})
	return res, err
}

// failCreateRejected sets a sandbox whose create the agent rejected to
// FAILED with CREATE_FAILED, which releases its reservation. A sandbox
// that the agent already reported past CREATING keeps its state.
func (s *Service) failCreateRejected(ctx context.Context, sandboxID string) error {
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := scanSandbox(tx.QueryRow(ctx, sandboxSelect+" WHERE s.sandbox_id = $1 FOR UPDATE OF s", sandboxID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.AgentState != ownsandv1.SandboxState_SANDBOX_STATE_CREATING {
			return nil
		}
		actor := r.deleteActor()
		if actor.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_UNSPECIFIED {
			actor = auth.SystemPrincipal
		}
		return s.endSandboxTx(ctx, tx, r, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED, actor)
	})
}
