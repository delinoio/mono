// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"context"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

func (r *Runtime) CreateWithRestoreClockForTest(ctx context.Context, spec vmruntime.Spec, setClock func(context.Context, time.Time) error, unconfirmedStop error) (vmruntime.VM, error) {
	ops := r.createOps()
	ops.setClock = func(ctx context.Context, _ *vm, now time.Time) error { return setClock(ctx, now) }
	if unconfirmedStop != nil {
		ops.stopRestore = func(*vm) error { return unconfirmedStop }
	}
	return r.createVM(ctx, spec, ops)
}
