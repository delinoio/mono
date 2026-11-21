// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// OrganizationHandler serves OrganizationService.
type OrganizationHandler struct{ *Service }

var _ ownsandv1connect.OrganizationServiceHandler = OrganizationHandler{}

func organizationProto(id, name string, created time.Time, role ownsandv1.Role) *ownsandv1.Organization {
	return &ownsandv1.Organization{OrganizationId: id, Name: name, CreateTime: ts(created), CallerRole: role}
}

func (h OrganizationHandler) CreateOrganization(ctx context.Context, req *connect.Request[ownsandv1.CreateOrganizationRequest]) (*connect.Response[ownsandv1.CreateOrganizationResponse], error) {
	p, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	orgID := newID()
	now := h.now()
	setRPCOrganization(ctx, orgID)
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, $2, $3)", orgID, name, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, $3, $4, $5)",
			newID(), orgID, p.ID, int16(ownsandv1.Role_ROLE_OWNER), now); err != nil {
			return err
		}
		// A new organization has no billing link: it is blocked with
		// ACCOUNT_REQUIRED until its owner links a billing account.
		if err := billing.InsertStatus(ctx, tx, orgID, now); err != nil {
			return err
		}
		return h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: orgID,
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "create organization", err)
	}
	return connect.NewResponse(&ownsandv1.CreateOrganizationResponse{
		Organization: organizationProto(orgID, name, now, ownsandv1.Role_ROLE_OWNER),
	}), nil
}

func (h OrganizationHandler) GetOrganization(ctx context.Context, req *connect.Request[ownsandv1.GetOrganizationRequest]) (*connect.Response[ownsandv1.GetOrganizationResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	_, role, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadOrganization)
	if err != nil {
		return nil, err
	}
	var name string
	var created time.Time
	err = h.DB.Pool.QueryRow(ctx, "SELECT name, create_time FROM organizations WHERE organization_id = $1", orgID).Scan(&name, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("organization")
	}
	if err != nil {
		return nil, h.internal(ctx, "get organization", err)
	}
	return connect.NewResponse(&ownsandv1.GetOrganizationResponse{Organization: organizationProto(orgID, name, created, role)}), nil
}

func (h OrganizationHandler) ListOrganizations(ctx context.Context, req *connect.Request[ownsandv1.ListOrganizationsRequest]) (*connect.Response[ownsandv1.ListOrganizationsResponse], error) {
	p, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT o.organization_id::text, o.name, o.create_time, m.role
		FROM memberships m JOIN organizations o ON o.organization_id = m.organization_id
		WHERE m.user_id = $1 AND o.organization_id > $2
		ORDER BY o.organization_id LIMIT $3`, p.ID, afterKey(after), limit)
	if err != nil {
		return nil, h.internal(ctx, "list organizations", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListOrganizationsResponse{}
	last := ""
	for rows.Next() {
		var id, name string
		var created time.Time
		var role int16
		if err := rows.Scan(&id, &name, &created, &role); err != nil {
			return nil, h.internal(ctx, "list organizations", err)
		}
		resp.Organizations = append(resp.Organizations, organizationProto(id, name, created, ownsandv1.Role(role)))
		last = id
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list organizations", err)
	}
	resp.NextPageToken = nextPageToken(len(resp.Organizations), limit, last)
	return connect.NewResponse(resp), nil
}

func (h OrganizationHandler) DeleteOrganization(ctx context.Context, req *connect.Request[ownsandv1.DeleteOrganizationRequest]) (*connect.Response[ownsandv1.DeleteOrganizationResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	var removedDevices []string
	var unlink billing.Change
	var unlinkPending bool
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		// The organization lock also serializes the deletion with concurrent
		// creates in the organization and with an ownership transfer.
		p, _, err := h.authorizeOwnerLocked(ctx, tx, orgID)
		if err != nil {
			return err
		}
		// The billing link goes with the organization; usage that is still
		// in the outbox is reported, and its debt stays on the billing
		// account.
		if unlink, unlinkPending, err = h.removeBillingLinkTx(ctx, tx, orgID, billing.SourceDelete); err != nil {
			return err
		}
		removedDevices, err = h.deleteOrganizationTx(ctx, tx, orgID, p)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "delete organization", err)
	}
	for _, deviceID := range removedDevices {
		h.afterDeviceRemoved(ctx, deviceID)
	}
	h.finishBillingUnlink(ctx, orgID, unlink, unlinkPending)
	return connect.NewResponse(&ownsandv1.DeleteOrganizationResponse{}), nil
}

// authorizeOwnerLocked authorizes an owner-only RPC that takes the
// organization lock: DeleteOrganization and TransferOwnership. Both mutations
// decide on the owner role, so they take the same organization row lock
// before the authoritative owner check and keep it until the transaction
// ends. A mutation that commits first is then visible to the owner check of
// the other one (read committed gives each statement a fresh snapshot):
// after a transfer, the former owner's deletion gets permission_denied, and
// after a deletion, a transfer gets not_found.
//
// The first authorize call is only a pre-check, so that API keys, non-members
// and members without the owner role fail without queueing on the lock; its
// result is not authoritative and is checked again after the lock.
func (s *Service) authorizeOwnerLocked(ctx context.Context, tx pgx.Tx, orgID string) (auth.Principal, ownsandv1.Role, error) {
	if _, _, err := s.authorize(ctx, tx, orgID, authz.ActionOwnerOnly); err != nil {
		return auth.Principal{}, 0, err
	}
	if err := lockOrganization(ctx, tx, orgID); err != nil {
		return auth.Principal{}, 0, err
	}
	return s.authorize(ctx, tx, orgID, authz.ActionOwnerOnly)
}

// lockOrganization takes the row lock of the organization (held until the
// transaction ends). The query returns a row, so an organization that a
// concurrent deletion removed while the caller waited for the lock yields
// not_found and stops the transaction. orgID must be validated.
func lockOrganization(ctx context.Context, tx pgx.Tx, orgID string) error {
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("organization")
	}
	return err
}

// lockOrganizationShare takes FOR KEY SHARE on the organization row (held
// until the transaction ends). DeleteOrganization takes the row FOR UPDATE
// first and then updates the devices, sandboxes and snapshots of the
// organization, and the foreign key of every row that an RPC inserts for the
// organization takes FOR KEY SHARE on it. An RPC that locks child rows
// before its insert reaches that foreign key would wait for the deletion
// while the deletion waits for those child rows: a deadlock. Such an RPC
// calls this helper before it locks any child row, which gives it the same
// order as the deletion. Callers that only insert or update child rows share
// the lock with each other, so they still run concurrently. An organization
// that a concurrent deletion removed while the caller waited yields
// not_found and stops the transaction. orgID must be validated.
func lockOrganizationShare(ctx context.Context, tx pgx.Tx, orgID string) error {
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR KEY SHARE", orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("organization")
	}
	return err
}

// deleteOrganizationTx marks the organization deleted and deletes its
// records, except the device removal tombstones, the audit events and the
// R2 clean-up job state. It removes every device of the organization as
// RemoveDevice does, with ORGANIZATION_DELETED as the sandbox end reason,
// and, after the records are deleted, starts the R2 clean-up job of the
// organization prefix, which deletes its exported snapshots, retries until
// it succeeds and sweeps the prefix once more after every signed upload URL
// expired. Deleting the records in the same transaction stops every signed
// URL issuance for the organization. It returns the removed device IDs.
func (s *Service) deleteOrganizationTx(ctx context.Context, tx pgx.Tx, orgID string, actor auth.Principal) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT device_id::text FROM devices WHERE organization_id = $1 AND state <> $2",
		orgID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
	if err != nil {
		return nil, err
	}
	var devices []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		devices = append(devices, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, deviceID := range devices {
		if err := s.removeDeviceTx(ctx, tx, orgID, deviceID, actor, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_ORGANIZATION_DELETED, false); err != nil {
			return nil, err
		}
	}
	// Sandboxes on devices that were already removed are terminal; any
	// other non-terminal sandbox (none is expected) ends here too.
	if err := s.endSandboxesTx(ctx, tx, "organization_id = $1", []any{orgID}, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_ORGANIZATION_DELETED, actor); err != nil {
		return nil, err
	}
	if err := s.writeAudit(ctx, tx, auditEvent{
		OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED, Actor: actor,
		TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: orgID,
	}); err != nil {
		return nil, err
	}
	if err := events.Write(ctx, tx, events.Event{Kind: events.KindOrganizationDeleted, OrganizationID: orgID}); err != nil {
		return nil, err
	}
	// The foreign keys delete memberships, invitations, API keys, enrollment
	// tokens, sandboxes, snapshots, templates, idempotency keys and the
	// agent version policy with the organization.
	if _, err := tx.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", orgID); err != nil {
		return nil, err
	}
	// The job is recorded after the cascade: the DELETE waits for every
	// signing transaction that holds a snapshot row of the organization
	// (handleSignedURLRequest takes FOR SHARE), so final_sweep_after is
	// computed after the last URL that can exist for the prefix was signed
	// and is never earlier than its expiry.
	if err := s.startCleanupJobTx(ctx, tx, orgID, organizationKeyPrefix+orgID+"/"); err != nil {
		return nil, err
	}
	return devices, nil
}

// startCleanupJobTx records an R2 clean-up job for prefix.
func (s *Service) startCleanupJobTx(ctx context.Context, q pgx.Tx, orgID, prefix string) error {
	now := s.now()
	_, err := q.Exec(ctx, `INSERT INTO r2_cleanup_jobs (job_id, organization_id, prefix, create_time, final_sweep_after, next_attempt_time)
		VALUES ($1, $2, $3, $4, $5, $4)`, newID(), orgID, prefix, now, now.Add(signedURLLifetime))
	return err
}

func (h OrganizationHandler) TransferOwnership(ctx context.Context, req *connect.Request[ownsandv1.TransferOwnershipRequest]) (*connect.Response[ownsandv1.TransferOwnershipResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	target := req.Msg.GetUserId()
	if target == "" {
		return nil, errInvalidf("invalid user_id: is required")
	}
	var unlink billing.Change
	var unlinkPending bool
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorizeOwnerLocked(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if target == p.ID {
			return errInvalidf("invalid user_id: the caller is already the owner")
		}
		var membershipID string
		err = tx.QueryRow(ctx, "SELECT membership_id::text FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", orgID, target).Scan(&membershipID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("member")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", orgID, p.ID, int16(ownsandv1.Role_ROLE_ADMIN)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", orgID, target, int16(ownsandv1.Role_ROLE_OWNER)); err != nil {
			return err
		}
		// The billing link belongs to the former owner's billing account:
		// the organization stays blocked until the new owner links one.
		if unlink, unlinkPending, err = h.removeBillingLinkTx(ctx, tx, orgID, billing.SourceTransfer); err != nil {
			return err
		}
		return h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_OWNERSHIP_TRANSFERRED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID,
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "transfer ownership", err)
	}
	h.finishBillingUnlink(ctx, orgID, unlink, unlinkPending)
	return connect.NewResponse(&ownsandv1.TransferOwnershipResponse{}), nil
}
