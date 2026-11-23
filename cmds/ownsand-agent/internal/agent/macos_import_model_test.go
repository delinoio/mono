// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// modelChecker is a runtime that supports only the macOS hardware models in
// supported, so the test can run the native compatibility check of the
// import command on a Linux host.
type modelChecker struct {
	vmruntime.Runtime
	supported map[string]bool
}

func (m modelChecker) MacOSHardwareModelSupported(model []byte) bool {
	return m.supported[string(model)]
}

// TestImportChecksMacOSHardwareModelBeforeDownload checks that the agent
// keeps its own compatibility check for an imported macOS snapshot: the
// control plane may send an import to a Mac that does not report the
// snapshot's hardware model yet, so the agent rejects an unsupported model
// with failed_precondition before it downloads any part, and imports a
// supported model.
func TestImportChecksMacOSHardwareModelBeforeDownload(t *testing.T) {
	var downloads atomic.Int32
	f := newImportFixtureWith(t, func(r *http.Request) int {
		if r.Method == http.MethodGet {
			downloads.Add(1)
		}
		return 0
	}, func(o *Options) {
		o.Runtime = modelChecker{Runtime: o.Runtime, supported: map[string]bool{"model-a": true}}
	})
	macOS := func(id, model string) *ownsandv1.AgentCommand {
		cmd := f.stage(t, id, false)
		entry := cmd.GetImportSnapshot().GetSnapshot()
		entry.GuestOs, entry.MacosHardwareModel = ownsandv1.GuestOs_GUEST_OS_MACOS, []byte(model)
		return cmd
	}

	unsupported, _ := uuidv7.NewString()
	cmd := macOS(unsupported, "model-b") // staging downloads the exported parts itself
	before := downloads.Load()
	res := f.h.command(cmd)
	if res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("unsupported model import = %v, want failed_precondition", res)
	}
	if got := downloads.Load() - before; got != 0 {
		t.Fatalf("the rejected import downloaded %d parts", got)
	}

	supported, _ := uuidv7.NewString()
	if res := f.h.command(macOS(supported, "model-a")); res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("supported model import = %v", res)
	}
}
