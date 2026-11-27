// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type hyperVExitProbeOutbox chan *ownsandv1.SandboxStateEntry

func (o hyperVExitProbeOutbox) SendSandboxUpdate(entry *ownsandv1.SandboxStateEntry) bool {
	if entry.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		select {
		case o <- entry:
		default:
		}
	}
	return true
}

func TestHyperVDisconnectExitProbe(t *testing.T) {
	kernel := os.Getenv("OWNSAND_HV_TEST_KERNEL")
	rootfs := os.Getenv("OWNSAND_HV_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Fatal("set OWNSAND_HV_TEST_KERNEL and OWNSAND_HV_TEST_ROOTFS to isolated image fixtures")
	}
	if ok, detail := hyperVCheck(); !ok {
		t.Fatal(detail)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{Kernel: kernel, Rootfs: rootfs, VMsDir: st.Path(store.VMsDir), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})
	if ids, err := rt.Running(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("the probe requires an isolated host with no existing OwnSand compute systems, got %d systems, error %v", len(ids), err)
	}
	manager, err := sandboxes.New(t.Context(), sandboxes.Options{Store: st, Runtime: rt, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	outbox := make(hyperVExitProbeOutbox, 1)
	manager.SetOutbox(outbox)

	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entry, err := manager.Create(t.Context(), &ownsandv1.CreateSandboxCommand{
		SandboxId:   id,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:   &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 512, DiskGib: 2},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:     ownsandv1.GuestOs_GUEST_OS_LINUX,
		IdleTtl:     durationpb.New(time.Hour),
		MaxTtl:      durationpb.New(time.Hour),
		CreateTime:  timestamppb.New(now),
		MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits:      &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 16, VcpuOvercommitPercent: 100},
	})
	if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox create state = %v, error = %v", entry.GetState(), err)
	}
	runtime := rt.(*Runtime)
	runtime.mu.Lock()
	v := runtime.vms[id]
	runtime.mu.Unlock()
	if v == nil {
		t.Fatal("running sandbox has no tracked VM")
	}
	v.mu.Lock()
	sys := v.sys
	watchDone := v.watchDone
	v.mu.Unlock()
	if sys == nil {
		t.Fatal("running VM has no compute system")
	}

	// Redirect native callbacks for only this test VM. Deliver a synthetic
	// disconnect through a separate callback key. Native queries stay live.
	callbackMu.Lock()
	if callbackSys[sys.cbKey] != sys {
		callbackMu.Unlock()
		t.Fatal("the running VM has no registered callback")
	}
	masked := newSystem(sys.id, 0)
	callbackSys[sys.cbKey] = masked
	callbackNext++
	probeKey := callbackNext
	callbackSys[probeKey] = sys
	callbackMu.Unlock()
	t.Cleanup(func() {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		delete(callbackSys, probeKey)
		if callbackSys[sys.cbKey] == masked {
			callbackSys[sys.cbKey] = sys
		}
	})

	requireOpenSignal(t, sys.disconnected, "pre-injection disconnect")

	type observation struct {
		confirmed bool
		err       error
	}
	first := make(chan observation, 1)
	allowRetry := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var queries atomic.Int32
	sys.verifyExitFn = func(id string, deadline time.Time) (bool, error) {
		call := queries.Add(1)
		if call > 1 {
			select {
			case <-allowRetry:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		confirmed, err := verifySystemExit(id, deadline)
		if call == 1 {
			first <- observation{confirmed, err}
		}
		return confirmed, err
	}

	systemCallback(&hcsEvent{Type: hcsEventServiceDisconnect}, probeKey)
	select {
	case got := <-first:
		if got.confirmed || got.err != nil {
			t.Fatalf("first native query: confirmed = %v, error = %v, want an inconclusive running VM", got.confirmed, got.err)
		}
	case <-time.After(stopTimeout + 5*time.Second):
		t.Fatal("no result from first native status query")
	}
	props, err := sys.properties()
	if err != nil || props.State != "Running" {
		t.Fatalf("HCS VM state = %q, error = %v, want Running", props.State, err)
	}
	requireOpenSignal(t, v.Exited(), "VM exit")
	if entry := manager.StateEntry(id); entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("agent state = %v before termination, want RUNNING", entry.GetState())
	}
	t.Log("first native query left the sandbox RUNNING with its exit signal open")

	if err := sys.terminate(); err != nil {
		t.Fatal(err)
	}
	close(allowRetry)
	select {
	case <-v.Exited():
	case <-time.After(stopTimeout + 5*time.Second):
		t.Fatal("the native HCS retry did not confirm VM exit")
	}
	waitForSignal(t, watchDone, "exit watcher after native HCS retry")
	if queries.Load() < 2 {
		t.Fatalf("native status query count = %d, want at least two", queries.Load())
	}
	select {
	case entry := <-outbox:
		if entry.GetSandboxId() != id || entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
			t.Fatalf("unexpected FAILED status update: state = %v, reason = %v", entry.GetState(), entry.GetEndReason())
		}
		t.Log("native retry confirmed exit with callbacks withheld and the agent emitted FAILED / LOST_ON_DEVICE")
	case <-time.After(stopTimeout + 5*time.Second):
		t.Fatal("agent did not emit FAILED / LOST_ON_DEVICE")
	}
}
