// SPDX-License-Identifier: FSL-1.1-ALv2

// Package unavailable is a runtime that runs no sandbox. The macOS and
// Windows runtimes use it until their hypervisor bindings are implemented,
// so the agent still enrolls, connects and reports the hypervisor as
// unavailable instead of failing to start.
package unavailable

import (
	"context"
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Runtime reports one failing hypervisor check and rejects every create.
type Runtime struct {
	kind    config.RuntimeKind
	checkID string
	detail  string
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns a runtime that reports checkID as failed with detail.
func New(kind config.RuntimeKind, checkID, detail string) *Runtime {
	return &Runtime{kind: kind, checkID: checkID, detail: detail}
}

func (r *Runtime) err() error {
	return fmt.Errorf("%w: %s", vmruntime.ErrUnsupported, r.detail)
}

func (r *Runtime) Kind() config.RuntimeKind { return r.kind }
func (r *Runtime) Capabilities(context.Context) vmruntime.Capabilities {
	return vmruntime.Capabilities{Checks: []*ownsandv1.HypervisorCheck{{CheckId: r.checkID, Passed: false, Detail: r.detail}}}
}
func (r *Runtime) Create(context.Context, vmruntime.Spec) (vmruntime.VM, error) { return nil, r.err() }
func (r *Runtime) Pause(context.Context, vmruntime.VM) error                    { return r.err() }
func (r *Runtime) Resume(context.Context, vmruntime.VM) error                   { return r.err() }
func (r *Runtime) Snapshot(context.Context, vmruntime.VM, string) (vmruntime.SnapshotInfo, error) {
	return vmruntime.SnapshotInfo{}, r.err()
}
func (r *Runtime) Delete(context.Context, string) error      { return nil }
func (r *Runtime) Running(context.Context) ([]string, error) { return nil, nil }
func (r *Runtime) Close() error                              { return nil }
