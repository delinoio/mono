// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/dataplane"
)

// newDataPath builds the data path component of the agent: peer-to-peer
// sessions and the relay connection.
func newDataPath(a *agent.Agent) agent.DataPath {
	return dataplane.New(dataplane.Options{
		Dispatcher: a.Ops,
		Sandboxes:  a.Sandboxes,
		Auth:       a,
		HTTP:       a.Control().HTTP,
		APIURL:     a.Control().APIURL,
		Logger:     a.Logger(),
	})
}
