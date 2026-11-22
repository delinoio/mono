// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
	// GuestOSes, BaseImages and Checks are the capabilities of the last
	// capability report of this agent process and CapabilitiesAt is its
	// time; all are empty until the agent built its first report.
	GuestOSes      []string      `json:"guest_oses,omitempty"`
	BaseImages     []string      `json:"base_images,omitempty"`
	Checks         []StatusCheck `json:"checks,omitempty"`
	CapabilitiesAt time.Time     `json:"capabilities_at,omitzero"`
}

// StatusCheck is one hypervisor check of a capability report.
type StatusCheck struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// setCapabilities records the capabilities of a capability report in the
// status and writes the status file, so that `ownsand-agent status` shows
// the checks that decide which sandboxes the device can run.
func (a *Agent) setCapabilities(c *ownsandv1.DeviceCapabilities) {
	var guests, images []string
	for _, g := range c.GetGuestOses() {
		guests = append(guests, g.String())
	}
	for _, b := range c.GetBuiltBaseImages() {
		images = append(images, b.String())
	}
	checks := make([]StatusCheck, 0, len(c.GetHypervisorChecks()))
	for _, ch := range c.GetHypervisorChecks() {
		checks = append(checks, StatusCheck{ID: ch.GetCheckId(), Passed: ch.GetPassed(), Detail: ch.GetDetail()})
	}
	a.mu.Lock()
	a.status.GuestOSes, a.status.BaseImages, a.status.Checks = guests, images, checks
	a.status.CapabilitiesAt = time.Now()
	a.mu.Unlock()
	a.writeStatus()
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

// writeStatus writes the status file. It holds statusMu for the whole
// gather-and-save step, so stopStatusWriter waits for a write that is in
// flight, and it does nothing once the writer is stopped.
func (a *Agent) writeStatus() {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.statusStopped {
		return
	}
	if a.statusWriteHook != nil {
		a.statusWriteHook()
	}
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

// stopStatusWriter fences the status file: it waits for a write that is in
// flight, and every later writeStatus (from statusLoop or setConnected)
// does nothing. The device removal calls it before it deletes the state
// directory, because statusLoop keeps running during the removal and
// would otherwise re-create status.json after the delete.
func (a *Agent) stopStatusWriter() {
	a.statusMu.Lock()
	a.statusStopped = true
	a.statusMu.Unlock()
}

// resumeStatusWriter lifts the fence after a removal attempt that failed
// and left the agent enrolled, so that the status file renews again while
// the removal retries.
func (a *Agent) resumeStatusWriter() {
	a.statusMu.Lock()
	a.statusStopped = false
	a.statusMu.Unlock()
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
