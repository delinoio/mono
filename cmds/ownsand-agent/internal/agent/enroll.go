// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ErrAlreadyEnrolled reports a state directory with a device identity.
var ErrAlreadyEnrolled = errors.New("agent: this device is already enrolled; run `ownsand-agent reset` first")

// ErrEnrollTimeout reports that the control plane did not answer the Enroll
// call within the enrollment deadline. It also matches
// context.DeadlineExceeded.
var ErrEnrollTimeout = errors.New("enrollment timed out")

// Enroll creates the device key pair and enrolls the device with a
// one-time enrollment token. The key is written before the call, so a retry
// after a lost response uses the same key, which the control plane accepts
// with the same token until the token expires. The token is never written
// or logged. The Enroll call has the deadline cp.EnrollTimeout (30 seconds
// by default): a control plane that accepts the call and never answers ends
// it with ErrEnrollTimeout, and the pending key stays for a retry. An earlier
// deadline or cancellation of ctx ends the call at once, with its own error.
func Enroll(ctx context.Context, st *store.Store, cp *cpclient.Client, token, name string, logger *slog.Logger) (*store.Identity, error) {
	id, err := st.LoadIdentity()
	switch {
	case err == nil && id.DeviceID != "":
		return nil, ErrAlreadyEnrolled
	case err == nil:
		// A pending key of an interrupted enrollment.
	case errors.Is(err, store.ErrNotFound):
		_, seed, gerr := identity.Generate()
		if gerr != nil {
			return nil, gerr
		}
		id = &store.Identity{PrivateKeySeed: seed}
		if err := st.SaveIdentity(id); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	key, err := identity.FromSeed(id.PrivateKeySeed)
	if err != nil {
		return nil, err
	}
	rctx, cancel := cp.EnrollContext(ctx)
	defer cancel()
	resp, err := cp.Agent.Enroll(rctx, connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: token,
		PublicKey:       key.PublicKey(),
		Name:            name,
		HostOs:          sysinfo.HostOS(),
		Arch:            sysinfo.Arch(),
		AgentVersion:    version.Version,
	}))
	if err != nil {
		// The enrollment deadline ended the call, and not the caller's
		// context. The server sees the same deadline through the Connect
		// timeout header and can answer deadline_exceeded just before the
		// client timer fires, so that code counts as the deadline too.
		if ctx.Err() == nil && (errors.Is(rctx.Err(), context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded) {
			logger.Error("enrollment timed out", "timeout", cp.EnrollTimeout().String())
			return nil, fmt.Errorf("agent: enroll: no answer within %s: %w: %w", cp.EnrollTimeout(), ErrEnrollTimeout, context.DeadlineExceeded)
		}
		logger.Error("enrollment failed", "code", connect.CodeOf(err).String())
		return nil, fmt.Errorf("agent: enroll: %w", err)
	}
	// The identity with a device ID is the "enrolled" marker, so persist the
	// origin that run needs first. A failure at either write leaves the
	// pending-key identity, and a retry enrolls again with the same key.
	if _, err := st.UpdateAgent(func(a *store.AgentState) error {
		a.APIURL = cp.APIURL
		a.Removal = store.RemovalRecord{}
		return nil
	}); err != nil {
		return nil, err
	}
	id.DeviceID = resp.Msg.GetDeviceId()
	id.OrganizationID = resp.Msg.GetOrganizationId()
	if err := st.SaveIdentity(id); err != nil {
		return nil, err
	}
	logger.Info("enrolled", "device_id", id.DeviceID, "organization_id", id.OrganizationID)
	return id, nil
}
