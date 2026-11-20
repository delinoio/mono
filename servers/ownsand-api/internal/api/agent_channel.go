// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Channel is not served yet: the agent channel lands with the agent
// channel implementation.
func (h AgentHandler) Channel(context.Context, *connect.BidiStream[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("the agent channel is not implemented yet"))
}
