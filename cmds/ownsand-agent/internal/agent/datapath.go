// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Channel is the agent channel connection that a data path component binds
// its sessions and relay grants to.
type Channel interface {
	// Generation identifies the connection; it increases with each new
	// agent channel.
	Generation() uint64
	// Send queues an envelope on this channel. It returns false when the
	// channel closed.
	Send(*ownsandv1.AgentEnvelope) bool
	// Done is closed when the channel closed.
	Done() <-chan struct{}
}

// DataPath serves the peer-to-peer sessions and relay streams. Every
// session and relay grant is bound to the agent channel that signaled or
// sent it.
type DataPath interface {
	// ChannelOpened binds a new agent channel.
	ChannelOpened(ch Channel)
	// ChannelLost ends every session and relay grant of the channel with
	// AGENT_CHANNEL_LOST.
	ChannelLost(ch Channel)
	// Configure applies the STUN URLs of a configuration message.
	Configure(stunURLs []string)
	// Offer answers a signaling offer.
	Offer(ctx context.Context, ch Channel, offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer
	// Revoke ends the session or the relay streams of the grant that the
	// revocation names.
	Revoke(ch Channel, rev *ownsandv1.SessionRevocation)
	// Grant installs a relay grant and returns its acknowledgement, or nil
	// when the grant is not acknowledged.
	Grant(ch Channel, grant *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck
	// Close ends everything.
	Close()
}

// NoDataPath rejects every session and does not acknowledge relay grants.
// It is the data path of an agent built without the data path component.
type NoDataPath struct{}

func (NoDataPath) ChannelOpened(Channel) {}
func (NoDataPath) ChannelLost(Channel)   {}
func (NoDataPath) Configure([]string)    {}
func (NoDataPath) Offer(_ context.Context, _ Channel, offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
	return &ownsandv1.SignalingAnswer{SessionId: offer.GetSessionId(), Error: &ownsandv1.ErrorDetail{
		Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "this agent has no data path",
	}}
}
func (NoDataPath) Revoke(Channel, *ownsandv1.SessionRevocation) {}
func (NoDataPath) Grant(Channel, *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
	return nil
}
func (NoDataPath) Close() {}
