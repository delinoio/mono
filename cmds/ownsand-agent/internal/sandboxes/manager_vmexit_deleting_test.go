// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// stopVM stops the fake VM of the sandbox as a VM that exits on its own.
func (e *env) stopVM(id string) {
	e.t.Helper()
	e.rt.mu.Lock()
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
}

// TestVMExitAfterFailedCallerDeleteEndsDeletedByCaller checks that a VM exit
// while a failed caller delete leaves the record in DELETING completes the
// delete with the recorded reason and does not report a lost VM.
func TestVMExitAfterFailedCallerDeleteEndsDeletedByCaller(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.failCallerDelete(id, false)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason %v, want DELETED_BY_CALLER", r)
	}
	e.ex.mu.Lock()
	r := e.ex.ended[id]
	e.ex.mu.Unlock()
	if r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("exec tracker end reason %v, want DELETED_BY_CALLER", r)
	}
	if n := e.ex.endedCalls(id); n != 1 {
		t.Fatalf("exec tracker notified %d times, want 1", n)
	}
}

// TestVMExitAfterFailedCallerDeleteKeepsReasonWhenDeadlinePassed checks that a
// passed deadline does not replace the caller reason recorded by the failed
// delete when the VM exits.
func TestVMExitAfterFailedCallerDeleteKeepsReasonWhenDeadlinePassed(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.failCallerDelete(id, false)
	e.clk.Advance(61 * time.Minute)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason %v, want DELETED_BY_CALLER", r)
	}
}

// TestVMExitAfterPassedIdleDeadlineEndsWithTTLReason checks that a running
// sandbox whose VM exits after the idle deadline passed ends with the TTL
// reason and not LOST_ON_DEVICE.
func TestVMExitAfterPassedIdleDeadlineEndsWithTTLReason(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.clk.Advance(61 * time.Minute)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("end reason %v, want IDLE_TTL_EXPIRED", r)
	}
}
