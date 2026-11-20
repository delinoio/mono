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
)

// Event is one control event.
type Event struct {
	ID             int64
	Kind           Kind
	OrganizationID string
	DeviceID       string
	SandboxID      string
	APIKeyID       string
	UserID         string
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Write inserts e. Call it inside the transaction of the change.
func Write(ctx context.Context, q db.Querier, e Event) error {
	_, err := q.Exec(ctx, `INSERT INTO control_events (kind, organization_id, device_id, sandbox_id, api_key_id, user_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		int16(e.Kind), nullable(e.OrganizationID), nullable(e.DeviceID), nullable(e.SandboxID), nullable(e.APIKeyID), nullable(e.UserID))
	return err
}

// LatestID returns the ID of the newest event, or 0.
func LatestID(ctx context.Context, q db.Querier) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, "SELECT COALESCE(max(event_id), 0) FROM control_events").Scan(&id)
	return id, err
}

// After returns up to limit events with an ID greater than after, oldest
// first.
func After(ctx context.Context, q db.Querier, after int64, limit int) ([]Event, error) {
	rows, err := q.Query(ctx, `SELECT event_id, kind, COALESCE(organization_id::text, ''), COALESCE(device_id::text, ''),
		COALESCE(sandbox_id::text, ''), COALESCE(api_key_id::text, ''), COALESCE(user_id, '')
		FROM control_events WHERE event_id > $1 ORDER BY event_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var kind int16
		if err := rows.Scan(&e.ID, &kind, &e.OrganizationID, &e.DeviceID, &e.SandboxID, &e.APIKeyID, &e.UserID); err != nil {
			return nil, err
		}
		e.Kind = Kind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}
