package runtime

import (
	"context"
	"io"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// unsupportedRuntime reports every guest as unsupported. The macOS and
// Windows agents use it until their hypervisor bindings are approved and
// implemented: they report no guest OS and no base image, so the scheduler
// never places a sandbox on them, and every VM operation fails with
// unimplemented.
type unsupportedRuntime struct {
	name   string
	checks map[string]string
}

func (u *unsupportedRuntime) Name() string { return u.name }

func (u *unsupportedRuntime) Capabilities(context.Context) Capabilities {
	return Capabilities{HypervisorChecks: u.checks}
}

func (u *unsupportedRuntime) Create(context.Context, VMSpec) error {
	return Unsupported("creating a VM")
}

func (u *unsupportedRuntime) Start(context.Context, string) (ownsandv1.RestorePath, error) {
	return 0, Unsupported("starting a VM")
}
func (u *unsupportedRuntime) Pause(context.Context, string) error { return Unsupported("pausing a VM") }
func (u *unsupportedRuntime) Resume(context.Context, string) error {
	return Unsupported("resuming a VM")
}
func (u *unsupportedRuntime) Stop(context.Context, string) error   { return nil }
func (u *unsupportedRuntime) Delete(context.Context, string) error { return nil }
func (u *unsupportedRuntime) Snapshot(context.Context, string, string) (SnapshotInfo, error) {
	return SnapshotInfo{}, Unsupported("taking a snapshot")
}
func (u *unsupportedRuntime) DeleteSnapshot(context.Context, string) error { return nil }
func (u *unsupportedRuntime) List(context.Context) ([]VMInfo, error)       { return nil, nil }
func (u *unsupportedRuntime) DialGuest(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, Unsupported("the guest channel")
}
func (u *unsupportedRuntime) DeleteAll(context.Context) error { return nil }
func (u *unsupportedRuntime) Close() error                    { return nil }
