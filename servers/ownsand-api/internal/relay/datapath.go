// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// DataPathService implements ownsandv1connect.DataPathServiceHandler.
type DataPathService struct{ M *Manager }

var _ ownsandv1connect.DataPathServiceHandler = DataPathService{}

// MaxSDPBytes bounds an SDP offer.
const MaxSDPBytes = 64 << 10

func (d DataPathService) GetIceServers(ctx context.Context, req *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
	if _, err := d.M.API.Authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.GetIceServersResponse{IceServers: d.M.IceServers}), nil
}

// sandboxForDataPath loads the sandbox and applies the state rule.
func (m *Manager) sandboxForDataPath(ctx context.Context, org, id string, allowPaused bool) (store.Sandbox, error) {
	sb, err := m.Store.GetSandbox(ctx, org, id)
	if errors.Is(err, store.ErrNotFound) {
		return sb, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	if err != nil {
		return sb, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	switch sb.State {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
	case ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
		if !allowPaused {
			return sb, connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is paused"))
		}
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return sb, connect.NewError(connect.CodeUnavailable, errors.New("the sandbox's device is offline"))
	default:
		return sb, connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is in "+sb.State.String()))
	}
	return sb, nil
}

func (d DataPathService) OpenSession(ctx context.Context, req *connect.Request[ownsandv1.OpenSessionRequest]) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
	m := d.M
	start := time.Now()
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.ErrMissingCredential
	}
	if !m.Hub.Ready() {
		return nil, errNoLease
	}
	if l := len(req.Msg.GetSdpOffer()); l == 0 || l > MaxSDPBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("sdp_offer is required and must be at most 64 KiB"))
	}
	// Register as pending before authorizing, so a revocation processed in
	// between ends this session too.
	pa := m.registerPending(p, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId())
	revokedErr := func() error {
		if m.finishPending(pa) {
			return errRevoked
		}
		return nil
	}
	if _, err := m.API.Authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		m.finishPending(pa)
		return nil, err
	}
	sb, err := m.sandboxForDataPath(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), false)
	if err != nil {
		m.finishPending(pa)
		return nil, err
	}
	c, ok := m.Hub.Live(sb.DeviceID)
	if !ok {
		m.finishPending(pa)
		return nil, errNoChannel
	}
	s := &session{id: datapath.NewID(), principal: p, org: sb.OrganizationID, sandbox: sb.ID, device: sb.DeviceID, gen: c.Generation, expiresAt: m.now().Add(m.Limits.SessionLifetime)}
	answer := make(chan *ownsandv1.SignalingAnswer, 1)
	m.mu.Lock()
	pa.device = sb.DeviceID
	m.sessions[s.id] = s
	m.signals[s.id] = answer
	m.mu.Unlock()
	cleanup := func() {
		m.mu.Lock()
		delete(m.sessions, s.id)
		delete(m.signals, s.id)
		m.mu.Unlock()
	}
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignalingOffer{SignalingOffer: &ownsandv1.SignalingOffer{
		SessionId: s.id, SandboxId: sb.ID, Principal: p.Proto(), ExpireTime: timestamppb.New(s.expiresAt), SdpOffer: req.Msg.GetSdpOffer(),
	}}})
	timer := time.NewTimer(m.Limits.SignalTimeout)
	defer timer.Stop()
	var ans *ownsandv1.SignalingAnswer
	select {
	case ans = <-answer:
	case <-c.Done():
		cleanup()
		m.finishPending(pa)
		return nil, errNoChannel
	case <-timer.C:
		cleanup()
		m.finishPending(pa)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the device agent did not answer the offer"))
	case <-ctx.Done():
		cleanup()
		m.finishPending(pa)
		return nil, ctx.Err()
	}
	if ans.GetErrorCode() != "" {
		cleanup()
		m.finishPending(pa)
		var code connect.Code
		if code.UnmarshalText([]byte(ans.GetErrorCode())) != nil {
			code = connect.CodeUnknown
		}
		return nil, connect.NewError(code, errors.New("the device agent rejected the session"))
	}
	if err := revokedErr(); err != nil {
		// The revocation already went to the agent with the session ID.
		return nil, err
	}
	m.Logger.Info("data_path_session", slog.String("session_id", s.id), slog.String("sandbox_id", sb.ID), slog.String("device_id", sb.DeviceID),
		slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String()), slog.Duration("duration", time.Since(start)))
	return connect.NewResponse(&ownsandv1.OpenSessionResponse{SessionId: s.id, SdpAnswer: ans.GetSdpAnswer(), ExpireTime: timestamppb.New(s.expiresAt)}), nil
}

func invalid(msg string) error { return connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) }

// ValidateAuthorize checks an AuthorizeRelay request.
func ValidateAuthorize(r *ownsandv1.AuthorizeRelayRequest) error {
	if _, err := datapath.ParseUUIDv7(r.GetAuthorizationId()); err != nil {
		return invalid("authorization_id must be a UUID v7")
	}
	if _, err := datapath.ParseUUIDv7(r.GetOperationId()); err != nil {
		return invalid("operation_id must be a UUID v7")
	}
	switch r.GetOperationKind() {
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC:
		if len(r.GetRequestDigest()) != 0 {
			return invalid("request_digest must be absent for ATTACH_EXEC")
		}
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE:
		if len(r.GetRequestDigest()) != 32 {
			return invalid("request_digest must be a 32-byte SHA-256")
		}
	default:
		return invalid("operation_kind is required")
	}
	if r.ReplacesGrantId != nil && !store.ValidID(r.GetReplacesGrantId()) {
		return invalid("replaces_grant_id must be a UUID")
	}
	return nil
}

func (m *Manager) grantResponse(g *grant) *connect.Response[ownsandv1.AuthorizeRelayResponse] {
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{
		GrantId: g.id, ExpiresAt: timestamppb.New(g.expiresAt), ExpiresIn: durationpb.New(g.expiresAt.Sub(m.now())),
	})
}

func (d DataPathService) AuthorizeRelay(ctx context.Context, req *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	m := d.M
	start := time.Now()
	r := req.Msg
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.ErrMissingCredential
	}
	if ok, delay := m.authorizeRate.Reserve(principalKey(p), 1); !ok {
		return nil, retryError("too many AuthorizeRelay calls; retry later", max(delay, m.Limits.RetryDelay))
	}
	if err := ValidateAuthorize(r); err != nil {
		return nil, err
	}
	if !m.Hub.Ready() {
		return nil, errNoLease
	}
	pa := m.registerPending(p, r.GetOrganizationId(), r.GetSandboxId())
	g, err := m.authorizeRelay(ctx, p, r, pa)
	if m.finishPending(pa) && err == nil {
		err = errRevoked
	}
	code := "ok"
	grantID := ""
	if err != nil {
		code = connect.CodeOf(err).String()
	} else {
		grantID = g.id
	}
	m.Logger.Info("relay_authorized", slog.String("grant_id", grantID), slog.String("sandbox_id", r.GetSandboxId()),
		slog.String("operation_kind", r.GetOperationKind().String()), slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY.String()),
		slog.String("code", code), slog.Duration("duration", time.Since(start)))
	if err != nil {
		return nil, err
	}
	return m.grantResponse(g), nil
}

func (m *Manager) authorizeRelay(ctx context.Context, p auth.Principal, r *ownsandv1.AuthorizeRelayRequest, pa *pendingAuth) (*grant, error) {
	if _, err := m.API.Authorize(ctx, r.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	allowPaused := r.GetOperationKind() == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || r.GetOperationKind() == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC
	sb, err := m.sandboxForDataPath(ctx, r.GetOrganizationId(), r.GetSandboxId(), allowPaused)
	if err != nil {
		return nil, err
	}
	c, ok := m.Hub.Live(sb.DeviceID)
	if !ok {
		return nil, errNoChannel
	}
	authKey := principalKey(p) + "/" + r.GetAuthorizationId()
	now := m.now()

	m.mu.Lock()
	pa.device = sb.DeviceID
	// Idempotency: a retry of a known request returns its grant.
	if g := m.byAuth[authKey]; g != nil && m.grants[g.id] == g {
		if !sameOperation(g, p, sb.ID, r.GetOperationKind(), r.GetOperationId(), r.GetRequestDigest()) || g.org != sb.OrganizationID {
			m.mu.Unlock()
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("authorization_id was used for another request"))
		}
		if g.revoked {
			m.mu.Unlock()
			return nil, errRevoked
		}
		m.mu.Unlock()
		return m.awaitGrant(ctx, g)
	}
	// Replacement of a revocable grant of the same principal and operation.
	var pred *grant
	if r.ReplacesGrantId != nil {
		if old := m.grants[r.GetReplacesGrantId()]; old != nil && m.revocableAt(old, now) &&
			sameOperation(old, p, sb.ID, r.GetOperationKind(), r.GetOperationId(), r.GetRequestDigest()) && old.org == sb.OrganizationID {
			if old.successor != nil && m.grants[old.successor.id] == old.successor {
				m.mu.Unlock()
				return nil, connect.NewError(connect.CodeAborted, errors.New("another replacement of this grant is in progress; retry"))
			}
			pred = old
		}
	}
	if pred == nil {
		dev, prin := m.countsLocked(sb.DeviceID, p, now)
		if dev >= m.Limits.GrantsPerDevice || prin >= m.Limits.GrantsPerPrincipal {
			m.mu.Unlock()
			return nil, retryError("too many relay grants; retry later", m.Limits.RetryDelay)
		}
	}
	g := &grant{
		id: datapath.NewID(), authorizationID: r.GetAuthorizationId(), principal: p, org: sb.OrganizationID, sandbox: sb.ID, device: sb.DeviceID,
		gen: c.Generation, kind: r.GetOperationKind(), opID: r.GetOperationId(), digest: bytes.Clone(r.GetRequestDigest()),
		expiresAt: now.Add(m.Limits.GrantExpiry), streamDeadline: now.Add(m.Limits.StreamDeadline), deadlineIn: m.Limits.StreamDeadline,
		decided: make(chan struct{}), replaces: pred,
	}
	if pred != nil {
		pred.successor = g
	}
	m.grants[g.id] = g
	m.byAuth[authKey] = g
	m.mu.Unlock()

	msg := &ownsandv1.RelayGrant{
		GrantId: g.id, SandboxId: g.sandbox, OperationKind: g.kind, OperationId: g.opID, RequestDigest: g.digest, Principal: p.Proto(),
		ExpiresAt: timestamppb.New(g.expiresAt), StreamDeadline: timestamppb.New(g.streamDeadline),
		ExpiresIn: durationpb.New(m.Limits.GrantExpiry), StreamDeadlineIn: durationpb.New(m.Limits.StreamDeadline),
	}
	if pred != nil {
		id := pred.id
		msg.ReplacesGrantId = &id
	}
	if !c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrant{RelayGrant: msg}}) {
		m.mu.Lock()
		m.removeGrantLocked(g)
		m.mu.Unlock()
		return nil, errNoChannel
	}
	return m.awaitGrant(ctx, g)
}

// awaitGrant waits up to the acknowledgement timeout for the agent's
// decision. The pending grant stays registered after a timeout, so a late
// acknowledgement installs it and a retry with the same authorization_id
// returns it.
func (m *Manager) awaitGrant(ctx context.Context, g *grant) (*grant, error) {
	timer := time.NewTimer(m.Limits.GrantAckTimeout)
	defer timer.Stop()
	select {
	case <-g.decided:
	case <-timer.C:
		return nil, errGrantNotAcked
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if g.rejected {
		return nil, retryError("the device is at its relay grant limit; retry later", m.Limits.RetryDelay)
	}
	if !m.admittableAt(g, m.now()) {
		return nil, errGrantInvalid()
	}
	return g, nil
}
