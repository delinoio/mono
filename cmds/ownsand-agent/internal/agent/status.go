// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
)

// Status is the status file of the running agent, which
// `ownsand-agent status` prints.
type Status struct {
	DeviceID      string    `json:"device_id"`
	APIURL        string    `json:"api_url"`
	AgentVersion  string    `json:"agent_version"`
	Runtime       string    `json:"runtime"`
	Connected     bool      `json:"connected"`
	LastConnected time.Time `json:"last_connected,omitzero"`
	UpdateState   string    `json:"update_state"`
	UpdateTarget  string    `json:"update_target,omitempty"`
	Sandboxes     int       `json:"sandboxes"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (a *Agent) setConnected(connected bool) {
	a.mu.Lock()
	a.status.Connected = connected
	if connected {
		a.status.LastConnected = time.Now()
	}
	a.mu.Unlock()
	a.writeStatus()
}

func (a *Agent) writeStatus() {
	state, target := a.updater.State()
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()
	s.DeviceID = a.DeviceID()
	s.APIURL = a.cp.APIURL
	s.AgentVersion = version.Version
	s.Runtime = a.rt.Kind().String()
	s.UpdateState = state.String()
	s.UpdateTarget = target
	s.Sandboxes = len(a.Sandboxes.Entries())
	s.UpdatedAt = time.Now()
	if err := a.st.SaveStatus(s); err != nil {
		a.logger.Warn("write status file failed", "error", err)
	}
}

func (a *Agent) statusLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	a.writeStatus()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.writeStatus()
		}
	}
}
