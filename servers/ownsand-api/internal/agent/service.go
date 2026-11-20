// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/nonce"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// SessionLifetime is the lifetime of a device session credential.
const SessionLifetime = 15 * time.Minute

// Service implements ownsandv1connect.AgentServiceHandler.
type Service struct {
	Hub    *Hub
	Store  *store.Store
	Logger *slog.Logger
	Nonces nonce.Keys
	// Limits per device ID (CreateChallenge, Authenticate, ConfirmRemoval)
	// and per enrollment token display prefix (Enroll).
	DeviceLimit *ratelimit.Limiter
	EnrollLimit *ratelimit.Limiter
	Now         func() time.Time
}

var _ ownsandv1connect.AgentServiceHandler = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var (
	errEnrollRejected = connect.NewError(connect.CodeUnauthenticated, errors.New("the enrollment token is not valid"))
	errBadChallenge   = connect.NewError(connect.CodeUnauthenticated, errors.New("the challenge response is not valid"))
	errUnknownDevice  = connect.NewError(connect.CodeNotFound, errors.New("unknown device"))
	errRateLimited    = connect.NewError(connect.CodeResourceExhausted, errors.New("too many requests for this device; retry later"))
	errNotConfigured  = connect.NewError(connect.CodeUnavailable, errors.New("device authentication is not configured"))
)

func (s *Service) allowDevice(deviceID string) bool {
	return s.DeviceLimit == nil || s.DeviceLimit.Allow("device:"+deviceID)
}

// Enroll exchanges an enrollment token and a device public key for a device.
func (s *Service) Enroll(ctx context.Context, req *connect.Request[ownsandv1.EnrollRequest]) (*connect.Response[ownsandv1.EnrollResponse], error) {
	token := req.Msg.GetEnrollmentToken()
	if s.EnrollLimit != nil && !s.EnrollLimit.Allow("enroll:"+secret.DisplayPrefix(token)) {
		return nil, errRateLimited
	}
	if len(req.Msg.GetPublicKey()) != ed25519.PublicKeySize {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("public_key must be a raw 32-byte Ed25519 key"))
	}
	if secret.Classify(token) != secret.KindEnrollmentToken {
		return nil, errEnrollRejected
	}
	caps := req.Msg.GetCapabilities()
	if caps == nil {
		caps = &ownsandv1.DeviceCapabilities{}
	}
	deviceID, orgID, err := s.Store.Enroll(ctx, secret.Hash(token), req.Msg.GetPublicKey(), caps, store.NewID())
	if errors.Is(err, store.ErrEnrollmentRejected) {
		s.Logger.Info("agent_enroll_rejected")
		return nil, errEnrollRejected
	}
	if err != nil {
		s.Logger.Error("agent_enroll_failed", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	s.Logger.Info("agent_enrolled", slog.String("device_id", deviceID), slog.String("organization_id", orgID))
	return connect.NewResponse(&ownsandv1.EnrollResponse{DeviceId: deviceID, OrganizationId: orgID}), nil
}

// CreateChallenge returns a stateless nonce for any well-formed device ID.
func (s *Service) CreateChallenge(ctx context.Context, req *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	if len(s.Nonces.Current) == 0 {
		return nil, errNotConfigured
	}
	id := req.Msg.GetDeviceId()
	if !store.ValidID(id) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("device_id must be a UUID"))
	}
	if !s.allowDevice(id) {
		return nil, errRateLimited
	}
	n, exp, err := s.Nonces.Issue(id, s.now())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&ownsandv1.CreateChallengeResponse{Nonce: n, ExpireTime: timestamppb.New(exp)}), nil
}

// consumeNonce checks the nonce MAC, device and expiry and records it.
func (s *Service) consumeNonce(ctx context.Context, deviceID string, n []byte) error {
	if len(s.Nonces.Current) == 0 {
		return errNotConfigured
	}
	exp, err := s.Nonces.Check(n, deviceID, s.now())
	if err != nil {
		return errBadChallenge
	}
	err = s.Store.ConsumeNonce(ctx, nonce.Hash(n), exp)
	if errors.Is(err, store.ErrAlreadyExists) {
		return errBadChallenge
	}
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return nil
}

// deviceKey returns the public key of an enrolled device, or of a removed
// device's live tombstone (removed=true), or errUnknownDevice.
func (s *Service) deviceKey(ctx context.Context, deviceID string) (key []byte, removed bool, err error) {
	d, err := s.Store.GetDeviceByID(ctx, deviceID)
	switch {
	case err == nil && d.State != ownsandv1.DeviceState_DEVICE_STATE_REMOVED:
		return d.PublicKey, false, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, false, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	key, ok, err := s.Store.LiveTombstone(ctx, deviceID)
	if err != nil {
		return nil, false, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if !ok {
		return nil, false, errUnknownDevice
	}
	return key, true, nil
}

// Authenticate exchanges a signed nonce for a device session credential, or
// returns the removal notice to a removed device.
func (s *Service) Authenticate(ctx context.Context, req *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	id := req.Msg.GetDeviceId()
	if !s.allowDevice(id) {
		return nil, errRateLimited
	}
	if err := s.consumeNonce(ctx, id, req.Msg.GetNonce()); err != nil {
		s.Logger.Info("agent_authenticate_rejected", slog.String("device_id", id), slog.String("reason", "nonce"))
		return nil, err
	}
	key, removed, err := s.deviceKey(ctx, id)
	if err != nil {
		s.Logger.Info("agent_authenticate_rejected", slog.String("device_id", id), slog.String("reason", connect.CodeOf(err).String()))
		return nil, err
	}
	if !ed25519.Verify(key, nonce.SignedMessage(nonce.LabelAuthenticate, req.Msg.GetNonce()), req.Msg.GetSignature()) {
		s.Logger.Info("agent_authenticate_rejected", slog.String("device_id", id), slog.String("reason", "signature"))
		return nil, errBadChallenge
	}
	if removed {
		s.Logger.Info("agent_authenticate_removal_notice", slog.String("device_id", id))
		return connect.NewResponse(&ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_RemovalNotice{RemovalNotice: &ownsandv1.RemovalNotice{}}}), nil
	}
	epoch := req.Msg.GetStateEpoch()
	if len(epoch) > 128 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("state_epoch is too long"))
	}
	cred := secret.New(secret.KindDeviceSession)
	exp := s.now().Add(SessionLifetime)
	err = s.Store.CreateDeviceSession(ctx, id, secret.Hash(cred), exp, epoch)
	if errors.Is(err, store.ErrConflict) {
		// Removed between the lookup and the insert.
		return connect.NewResponse(&ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_RemovalNotice{RemovalNotice: &ownsandv1.RemovalNotice{}}}), nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	s.Logger.Info("agent_authenticated", slog.String("device_id", id))
	return connect.NewResponse(&ownsandv1.AuthenticateResponse{
		Result:     &ownsandv1.AuthenticateResponse_DeviceSessionCredential{DeviceSessionCredential: cred},
		ExpireTime: timestamppb.New(exp),
	}), nil
}

// ConfirmRemoval confirms the clean-up of a removed device and deletes its
// tombstone. An enrolled device gets failed_precondition; an unknown device
// gets not_found, which the agent treats as a confirmed removal.
func (s *Service) ConfirmRemoval(ctx context.Context, req *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	id := req.Msg.GetDeviceId()
	if !s.allowDevice(id) {
		return nil, errRateLimited
	}
	if err := s.consumeNonce(ctx, id, req.Msg.GetNonce()); err != nil {
		return nil, err
	}
	key, removed, err := s.deviceKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key, nonce.SignedMessage(nonce.LabelConfirmRemoval, req.Msg.GetNonce()), req.Msg.GetSignature()) {
		return nil, errBadChallenge
	}
	if !removed {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the device is not removed"))
	}
	if err := s.Store.ConfirmRemoval(ctx, id); errors.Is(err, store.ErrNotFound) {
		return nil, errUnknownDevice
	} else if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	s.Logger.Info("agent_removal_confirmed", slog.String("device_id", id))
	return connect.NewResponse(&ownsandv1.ConfirmRemovalResponse{}), nil
}

// Channel serves the agent channel.
func (s *Service) Channel(ctx context.Context, stream *connect.BidiStream[ownsandv1.AgentEnvelope, ownsandv1.AgentEnvelope]) error {
	return s.Hub.Serve(ctx, stream)
}
