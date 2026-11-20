// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"

// newDataPath builds the data path component of the agent.
func newDataPath(*agent.Agent) agent.DataPath {
	return agent.NoDataPath{}
}
