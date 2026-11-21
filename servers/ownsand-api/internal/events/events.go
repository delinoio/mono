// SPDX-License-Identifier: FSL-1.1-ALv2

// Package events writes and reads the control events table. Any instance
// writes an event in the transaction of a request that revokes access or
// changes what a connected agent must receive; the lease holder processes
// the events (see the Agent channel and Data path sections of
// docs/servers-ownsand-api-foundation.md).
package events

import (
	"context"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// Kind is the kind of a control event.
type Kind int16

const (
	KindUnspecified Kind = iota
	// KindAPIKeyRevoked: APIKeyID. Revokes the sessions and relay grants of
	// the key.
	KindAPIKeyRevoked
	// KindMembershipEnded: OrganizationID and UserID. Revokes the sessions
	// and relay grants of the user in the organization.
	KindMembershipEnded
	// KindSandboxDeleted: SandboxID. Revokes the sessions and relay grants
	// of the sandbox.
	KindSandboxDeleted
	// KindDeviceConfigChanged: DeviceID. The device limits changed.
	KindDeviceConfigChanged
	// KindAgentPolicyChanged: OrganizationID. The agent version policy
	// changed.
	KindAgentPolicyChanged
	// KindSandboxDeleteRequested: SandboxID and DeviceID. A delete is
	// pending; send the delete command to a connected device.
	KindSandboxDeleteRequested
	// KindDeviceRemoved: DeviceID. Send the clean-up command and close the
	// channel.
	KindDeviceRemoved
	// KindOrganizationDeleted: OrganizationID. Revokes every session and
	// relay grant of the organization.
	KindOrganizationDeleted
	// KindSnapshotDeleted: DeviceID and SnapshotID. The snapshot record was
	// deleted; send the delete-snapshot command to a connected device that
	// holds a local copy.
	KindSnapshotDeleted
)

// Event is one control event.
type Event struct {
	// ID is the delivery ID of an event that After returns: the position of
	// the event in commit order, and the cursor of the poll. The database
	// assigns it when the transaction of the event commits, so it is not set
	// for an event that Write receives. It is not the event_id column, which
	// is allocated at insert time and can be out of commit order.
	ID             int64
	Kind           Kind
	OrganizationID string
	DeviceID       string
	SandboxID      string
	APIKeyID       string
	UserID         string
	SnapshotID     string
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Write inserts e. Call it inside the transaction of the change.
func Write(ctx context.Context, q db.Querier, e Event) error {
	_, err := q.Exec(ctx, `INSERT INTO control_events (kind, organization_id, device_id, sandbox_id, api_key_id, user_id, snapshot_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		int16(e.Kind), nullable(e.OrganizationID), nullable(e.DeviceID), nullable(e.SandboxID), nullable(e.APIKeyID), nullable(e.UserID), nullable(e.SnapshotID))
	return err
}

// LatestID returns the delivery ID of the newest committed event, or 0.
func LatestID(ctx context.Context, q db.Querier) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, "SELECT COALESCE(max(delivery_id), 0) FROM control_events").Scan(&id)
	return id, err
}

// After returns up to limit events with a delivery ID greater than after,
// in delivery order. A trigger assigns delivery IDs in commit order, so a
// poll that has seen delivery ID N has also seen every event with a lower one
// (migration 0006_control_events_delivery_id.sql).
func After(ctx context.Context, q db.Querier, after int64, limit int) ([]Event, error) {
	rows, err := q.Query(ctx, `SELECT delivery_id, kind, COALESCE(organization_id::text, ''), COALESCE(device_id::text, ''),
		COALESCE(sandbox_id::text, ''), COALESCE(api_key_id::text, ''), COALESCE(user_id, ''), COALESCE(snapshot_id::text, '')
		FROM control_events WHERE delivery_id > $1 ORDER BY delivery_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kind int16
		if err := rows.Scan(&e.ID, &kind, &e.OrganizationID, &e.DeviceID, &e.SandboxID, &e.APIKeyID, &e.UserID, &e.SnapshotID); err != nil {
			return nil, err
		}
		e.Kind = Kind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}
