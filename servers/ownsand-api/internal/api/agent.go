// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// AgentHandler serves the unary AgentService RPCs. Channel is served by the
// agent channel package and is added to this handler by AgentService.
type AgentHandler struct{ *Service }

// maxDeviceNameLength bounds the reported host name.
const maxDeviceNameLength = 253

// agentLimiterKey builds the per-device limiter key of an unary agent RPC.
// deviceID must be normalized so that every spelling of one device shares a
// bucket per operation.
func agentLimiterKey(op, deviceID string) string { return op + ":" + deviceID }

// limitAgentRPC applies the per-device (or per-prefix) and per-client
// limits of the unauthenticated device RPCs. The per-client key is the bare
// client address, so these RPCs, AcceptInvitation and failed API key
// validations share one bucket per address.
func (s *Service) limitAgentRPC(ctx context.Context, key string) error {
	if ok, retry := s.Limits.PerDevice.Allow(key, 1); !ok {
		return errRateLimited(retry)
	}
	if addr := clientAddr(ctx); addr != "" {
		if ok, retry := s.Limits.PerClient.Allow(addr, 1); !ok {
			return errRateLimited(retry)
		}
	}
	return nil
}

// errEnrollmentInvalid is the one answer for an unknown, expired, revoked
// or used enrollment token.
func errEnrollmentInvalid() error { return errUnauthenticated("invalid enrollment token") }

func (h AgentHandler) Enroll(ctx context.Context, req *connect.Request[ownsandv1.EnrollRequest]) (*connect.Response[ownsandv1.EnrollResponse], error) {
	m := req.Msg
	token := m.GetEnrollmentToken()
	if err := h.limitAgentRPC(ctx, "enroll:"+auth.DisplayPrefix(token)); err != nil {
		return nil, err
	}
	if len(m.GetPublicKey()) != ed25519.PublicKeySize {
		return nil, errInvalidf("invalid public_key: must be a 32-byte Ed25519 public key")
	}
	name := m.GetName()
	if strings.TrimSpace(name) == "" || len(name) > maxDeviceNameLength {
		return nil, errInvalidf("invalid name: must be 1 to %d bytes", maxDeviceNameLength)
	}
	// PostgreSQL text cannot store U+0000; reject it before the transaction so
	// that the enrollment token stays usable and the caller gets
	// invalid_argument instead of an internal error.
	if err := validate.NoNUL("name", name); err != nil {
		return nil, errInvalid(err)
	}
	if m.GetHostOs() == ownsandv1.HostOs_HOST_OS_UNSPECIFIED || ownsandv1.HostOs_name[int32(m.GetHostOs())] == "" {
		return nil, errInvalidf("invalid host_os: must be a host OS")
	}
	if m.GetArch() == ownsandv1.Arch_ARCH_UNSPECIFIED || ownsandv1.Arch_name[int32(m.GetArch())] == "" {
		return nil, errInvalidf("invalid arch: must be an architecture")
	}
	if v := m.GetAgentVersion(); v == "" || len(v) > maxVersionLength {
		return nil, errInvalidf("invalid agent_version: is required")
	}
	if err := validate.NoNUL("agent_version", m.GetAgentVersion()); err != nil {
		return nil, errInvalid(err)
	}
	if !auth.WellFormed(auth.SecretEnrollmentToken, token) {
		return nil, errEnrollmentInvalid()
	}
	hash := auth.HashSecret(token)
	var resp *ownsandv1.EnrollResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		// Lock order: organization, then token, the order that
		// DeleteOrganization follows (organization row lock, then the cascade
		// to its tokens). Step 1 resolves the organization without a lock, so
		// the transaction never holds the token lock while it waits for the
		// organization. Step 2 takes FOR KEY SHARE on the organization: it
		// lets enrollments of one organization run together, and it conflicts
		// with the FOR UPDATE of a deletion, so a deletion that already
		// listed the devices cannot miss this device, and an enrollment that
		// started after the deletion committed finds no organization. Step 3
		// locks the token and reads it again; everything is validated there.
		var preOrgID string
		err := tx.QueryRow(ctx, "SELECT organization_id::text FROM enrollment_tokens WHERE secret_sha256 = $1", hash).Scan(&preOrgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errEnrollmentInvalid()
		}
		if err != nil {
			return err
		}
		var locked int
		err = tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR KEY SHARE", preOrgID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			// The organization was deleted while this call waited for the lock.
			return errEnrollmentInvalid()
		}
		if err != nil {
			return err
		}
		var tokenID, orgID, deviceID string
		var stored, usedKey []byte
		var expires time.Time
		var used, revoked *time.Time
		err = tx.QueryRow(ctx, `SELECT enrollment_token_id::text, organization_id::text, secret_sha256, expire_time, use_time, revoke_time,
			COALESCE(device_id::text, ''), device_public_key FROM enrollment_tokens WHERE secret_sha256 = $1 FOR UPDATE`, hash).
			Scan(&tokenID, &orgID, &stored, &expires, &used, &revoked, &deviceID, &usedKey)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!auth.HashEqual(stored, hash) || orgID != preOrgID)) {
			return errEnrollmentInvalid()
		}
		if err != nil {
			return err
		}
		now := h.now()
		if revoked != nil || !now.Before(expires) {
			return errEnrollmentInvalid()
		}
		setRPCOrganization(ctx, orgID)
		// Billing enforcement for the token's organization; the token was
		// validated above, and no record changed yet.
		if err := h.checkBilling(ctx, tx, orgID, restrictWhenBlocked); err != nil {
			return err
		}
		if used != nil {
			// A retry with the same token and public key gets the same device,
			// so an agent that lost the first response can continue.
			if deviceID != "" && auth.HashEqual(usedKey, m.GetPublicKey()) {
				resp = &ownsandv1.EnrollResponse{DeviceId: deviceID, OrganizationId: orgID}
				return nil
			}
			return errEnrollmentInvalid()
		}
		deviceID = newID()
		if _, err := tx.Exec(ctx, `INSERT INTO devices (device_id, organization_id, public_key, name, host_os, arch, agent_version, state, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			deviceID, orgID, m.GetPublicKey(), name, int16(m.GetHostOs()), int16(m.GetArch()), m.GetAgentVersion(),
			int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE enrollment_tokens SET use_time = $2, device_id = $3, device_public_key = $4 WHERE enrollment_token_id = $1",
			tokenID, now, deviceID, m.GetPublicKey()); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_ENROLLED,
			Actor:      auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE, ID: deviceID},
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE, TargetID: deviceID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.EnrollResponse{DeviceId: deviceID, OrganizationId: orgID}
		return nil
	})
	if err != nil {
		h.Logger.InfoContext(ctx, "agent_enroll", slog.String("result", codeString(err)))
		return nil, h.internal(ctx, "enroll", err)
	}
	h.Logger.InfoContext(ctx, "agent_enroll", slog.String("result", "ok"), slog.String("device_id", resp.GetDeviceId()), slog.String("organization_id", resp.GetOrganizationId()))
	return connect.NewResponse(resp), nil
}

func (h AgentHandler) CreateChallenge(ctx context.Context, req *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	rawID := req.Msg.GetDeviceId()
	deviceID, err := normalizeID("device_id", rawID)
	if err != nil {
		return nil, err
	}
	if err := h.limitAgentRPC(ctx, agentLimiterKey("challenge", deviceID)); err != nil {
		return nil, err
	}
	nonce, expiry, err := h.Nonces.Issue(deviceID)
	if err != nil {
		return nil, h.internal(ctx, "issue nonce", err)
	}
	return connect.NewResponse(&ownsandv1.CreateChallengeResponse{Nonce: nonce, ExpireTime: ts(expiry)}), nil
}

// deviceAuthRecord is the part of a device row that Authenticate and
// ConfirmRemoval need.
type deviceAuthRecord struct {
	orgID     string
	publicKey []byte
	state     ownsandv1.DeviceState
	removed   *time.Time
}

// verifySignedNonce checks the nonce and the signature of a device RPC and
// consumes the nonce. An unknown device (also one whose tombstone expired)
// fails with not_found.
func (s *Service) verifySignedNonce(ctx context.Context, tx pgx.Tx, deviceID string, nonce, signature []byte, purpose string) (deviceAuthRecord, error) {
	var rec deviceAuthRecord
	expiry, err := s.Nonces.Check(deviceID, nonce)
	if err != nil {
		return rec, errUnauthenticated("invalid or expired nonce")
	}
	var state int16
	err = tx.QueryRow(ctx, `SELECT organization_id::text, public_key, state, remove_time FROM devices
		WHERE device_id = $1 AND (tombstone_expire_time IS NULL OR tombstone_expire_time > $2) FOR UPDATE`, deviceID, s.now()).
		Scan(&rec.orgID, &rec.publicKey, &state, &rec.removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, errNotFound("device")
	}
	if err != nil {
		return rec, err
	}
	rec.state = ownsandv1.DeviceState(state)
	if !auth.VerifyDeviceSignature(rec.publicKey, purpose, nonce, signature) {
		return rec, errUnauthenticated("invalid signature")
	}
	// Check ran before this transaction waited for the device row lock, so the
	// nonce can have expired meanwhile. Validate the deadline again at the
	// locked point, before the nonce is consumed.
	if auth.NonceExpired(s.now(), expiry) {
		return rec, errUnauthenticated("invalid or expired nonce")
	}
	tag, err := tx.Exec(ctx, "INSERT INTO consumed_nonces (nonce_sha256, expire_time) VALUES ($1, $2) ON CONFLICT DO NOTHING", auth.NonceHash(nonce), expiry)
	if err != nil {
		return rec, err
	}
	if tag.RowsAffected() == 0 {
		return rec, errUnauthenticated("nonce already used")
	}
	return rec, nil
}

func (h AgentHandler) Authenticate(ctx context.Context, req *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	m := req.Msg
	rawID := m.GetDeviceId()
	deviceID, err := normalizeID("device_id", rawID)
	if err != nil {
		return nil, err
	}
	if err := h.limitAgentRPC(ctx, agentLimiterKey("authenticate", deviceID)); err != nil {
		return nil, err
	}
	if !uuidv7.Valid(m.GetStateEpoch()) {
		return nil, errInvalidf("invalid state_epoch: must be a canonical UUID v7")
	}
	var resp *ownsandv1.AuthenticateResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		rec, err := h.verifySignedNonce(ctx, tx, deviceID, m.GetNonce(), m.GetSignature(), auth.PurposeAuthenticate)
		if err != nil {
			return err
		}
		setRPCOrganization(ctx, rec.orgID)
		if rec.state == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			resp = &ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_RemovalNotice{
				RemovalNotice: &ownsandv1.RemovalNotice{RemoveTime: tsPtr(rec.removed)},
			}}
			return nil
		}
		secret, err := auth.NewSecret(auth.SecretDeviceSession)
		if err != nil {
			return err
		}
		now := h.now()
		expires := now.Add(deviceSessionLifetime)
		if _, err := tx.Exec(ctx, "INSERT INTO device_sessions (credential_sha256, device_id, expire_time) VALUES ($1, $2, $3)", secret.Hash, deviceID, expires); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE devices SET state_epoch = $2 WHERE device_id = $1", deviceID, m.GetStateEpoch()); err != nil {
			return err
		}
		resp = &ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_Session{Session: &ownsandv1.DeviceSession{
			DeviceSessionCredential: secret.Value,
			ExpireTime:              ts(expires),
			ExpiresIn:               durationpb.New(deviceSessionLifetime),
			OrganizationId:          rec.orgID,
		}}}
		return nil
	})
	if err != nil {
		h.Logger.InfoContext(ctx, "agent_authenticate", slog.String("device_id", deviceID), slog.String("result", codeString(err)))
		return nil, h.internal(ctx, "authenticate", err)
	}
	result := "session"
	if resp.GetRemovalNotice() != nil {
		result = "removal_notice"
	}
	h.Logger.InfoContext(ctx, "agent_authenticate", slog.String("device_id", deviceID), slog.String("result", result))
	return connect.NewResponse(resp), nil
}

func (h AgentHandler) ConfirmRemoval(ctx context.Context, req *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	m := req.Msg
	rawID := m.GetDeviceId()
	deviceID, err := normalizeID("device_id", rawID)
	if err != nil {
		return nil, err
	}
	if err := h.limitAgentRPC(ctx, agentLimiterKey("confirm-removal", deviceID)); err != nil {
		return nil, err
	}
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		rec, err := h.verifySignedNonce(ctx, tx, deviceID, m.GetNonce(), m.GetSignature(), auth.PurposeConfirmRemoval)
		if err != nil {
			return err
		}
		setRPCOrganization(ctx, rec.orgID)
		if rec.state != ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return errPrecondition("the device is not removed")
		}
		// Deleting the tombstone also ends the macOS machine identifier
		// reservation of the device's sandboxes that ended with the removal.
		_, err = tx.Exec(ctx, "DELETE FROM devices WHERE device_id = $1", deviceID)
		return err
	})
	if err != nil {
		h.Logger.InfoContext(ctx, "agent_confirm_removal", slog.String("device_id", deviceID), slog.String("result", codeString(err)))
		return nil, h.internal(ctx, "confirm removal", err)
	}
	h.Logger.InfoContext(ctx, "agent_confirm_removal", slog.String("device_id", deviceID), slog.String("result", "ok"))
	return connect.NewResponse(&ownsandv1.ConfirmRemovalResponse{}), nil
}
