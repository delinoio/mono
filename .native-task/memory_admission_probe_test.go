// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/go/uuidv7"
	v1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type admissionProbeRuntime struct {
	vmruntime.Runtime
	kind   config.RuntimeKind
	ids    [2]string
	starts int
}

func (r *admissionProbeRuntime) Kind() config.RuntimeKind { return r.kind }

func (r *admissionProbeRuntime) Running(ctx context.Context) ([]string, error) {
	ids, err := r.Runtime.Running(ctx)
	// Reconcile only this probe's sandbox IDs, so the manager cannot stop
	// an existing HCS VM that belongs to another agent.
	return slices.DeleteFunc(ids, func(id string) bool {
		return !slices.Contains(r.ids[:], id)
	}), err
}

func (r *admissionProbeRuntime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	r.starts++
	return r.Runtime.Create(ctx, spec)
}

func TestRuntimeMemoryAdmissionProbe(t *testing.T) {
	if os.Getenv("OWNSAND_MEMORY_ADMISSION_PROBE") != "1" {
		t.Fatal("set OWNSAND_MEMORY_ADMISSION_PROBE=1 to run with a disposable runtime")
	}
	kinds := []config.RuntimeKind{config.RuntimeKindHyperV}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, config.RuntimeKindProcess)
	}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) { probeMemoryAdmission(t, kind) })
	}
}

func probeMemoryAdmission(t *testing.T, kind config.RuntimeKind) {
	ctx := t.Context()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var rt vmruntime.Runtime
	diskGiB := int64(2)
	if runtime.GOOS == "windows" {
		kernel, rootfs := os.Getenv("OWNSAND_HV_TEST_KERNEL"), os.Getenv("OWNSAND_HV_TEST_ROOTFS")
		if _, err := os.Stat(kernel); err != nil {
			t.Fatalf("provide OWNSAND_HV_TEST_KERNEL: %v", err)
		}
		fi, err := os.Stat(rootfs)
		if err != nil {
			t.Fatalf("provide OWNSAND_HV_TEST_ROOTFS: %v", err)
		}
		diskGiB = max(diskGiB, (fi.Size()+(1<<30)-1)>>30)
		rt, err = hcsrt.New(hcsrt.Options{Kernel: kernel, Rootfs: rootfs, VMsDir: st.Path(store.VMsDir)})
		if err != nil {
			t.Fatal(err)
		}
		t.Log("using native HCS")
	} else {
		rt, err = processrt.NewWithOptions(processrt.Options{
			GuestBinary: os.Getenv("OWNSAND_MEMORY_PROBE_GUEST"),
			VMsDir:      st.Path(store.VMsDir),
			Logger:      slog.Default(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("using real guest helper processes and runtime kind %s", kind)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Error(err)
		}
	})
	probe := &admissionProbeRuntime{Runtime: rt, kind: kind}
	for i := range probe.ids {
		probe.ids[i], err = uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := sandboxes.New(ctx, sandboxes.Options{Store: st, Runtime: probe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	scheduled := &v1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, DiskGib: diskGiB * 2, VcpuOvercommitPercent: 100}
	newCommand := func(id string, memoryMiB int64) *v1.CreateSandboxCommand {
		now := time.Now()
		return &v1.CreateSandboxCommand{
			SandboxId:   id,
			Resources:   &v1.Resources{VcpuCount: 1, MemoryMib: memoryMiB, DiskGib: diskGiB},
			Source:      &v1.CreateSandboxCommand_BaseImage{BaseImage: v1.BaseImage_BASE_IMAGE_LINUX},
			GuestOs:     v1.GuestOs_GUEST_OS_LINUX,
			NetworkMode: v1.NetworkMode_NETWORK_MODE_NONE,
			IdleTtl:     durationpb.New(time.Hour),
			MaxTtl:      durationpb.New(time.Hour),
			CreateTime:  timestamppb.New(now),
			MaxDeadline: timestamppb.New(now.Add(time.Hour)),
			Limits:      scheduled,
		}
	}
	create := func(cmd *v1.CreateSandboxCommand) {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		entry, err := m.Create(bootCtx, cmd)
		if err != nil || entry.GetState() != v1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatalf("create %d MiB: entry %v, error %v", cmd.GetResources().GetMemoryMib(), entry, err)
		}
		data, err := os.ReadFile(st.Path(store.SandboxesDir, cmd.GetSandboxId()+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var stored sandboxes.Record
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.MemoryMiB != cmd.GetResources().GetMemoryMib() {
			t.Fatalf("persisted memory %d MiB, want requested %d MiB", stored.MemoryMiB, cmd.GetResources().GetMemoryMib())
		}
		client, ok := m.Guest(cmd.GetSandboxId())
		if !ok {
			t.Fatal("missing guest client")
		}
		if _, err := client.CheckReady(bootCtx); err != nil {
			t.Fatalf("guest readiness: %v", err)
		}
	}

	first, next := newCommand(probe.ids[0], 511), newCommand(probe.ids[1], 512)
	m.SetLimits(scheduled)
	create(first)
	t.Log("first guest requested 511 MiB and answered CheckReady")
	boundary := int64(1024)
	if kind == config.RuntimeKindProcess {
		boundary = 1023
	}
	latest := proto.CloneOf(scheduled)
	latest.MemoryMib = boundary - 1
	m.SetLimits(latest)
	entry, err := m.Create(ctx, next)
	var ce *sandboxes.CommandError
	if entry != nil || !errors.As(err, &ce) || ce.Code != v1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED || ce.Message != "device memory limit reached" {
		t.Fatalf("tight limit %d MiB: entry %v, error %v", latest.MemoryMib, entry, err)
	}
	if probe.starts != 1 {
		t.Fatalf("runtime starts %d, want 1", probe.starts)
	}
	if _, found := m.State(next.GetSandboxId()); found {
		t.Fatal("rejected sandbox has an in-memory record")
	}
	for _, path := range []string{st.Path(store.SandboxesDir, next.GetSandboxId()+".json"), st.Path(store.VMsDir, next.GetSandboxId())} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected sandbox path must be absent: %v", err)
		}
	}
	ids, err := probe.Running(ctx)
	if err != nil || len(ids) != 1 || ids[0] != first.GetSandboxId() {
		t.Fatalf("expected only the first sandbox in runtime inventory, found %v, error %v", ids, err)
	}
	t.Logf("%d MiB limit rejected the second guest before record creation or runtime start", latest.MemoryMib)

	latest = proto.CloneOf(scheduled)
	latest.MemoryMib = boundary
	m.SetLimits(latest)
	create(next)
	ids, err = probe.Running(ctx)
	if err != nil || len(ids) != 2 || probe.starts != 2 {
		t.Fatalf("expected both sandboxes in runtime inventory, found %v, starts %d, error %v", ids, probe.starts, err)
	}
	t.Logf("%d MiB limit admitted the same request and both guests are present", boundary)
	for _, id := range probe.ids {
		entry, err := m.Delete(ctx, &v1.DeleteSandboxCommand{SandboxId: id})
		if err != nil || entry.GetState() != v1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete test sandbox: entry %v, error %v", entry, err)
		}
	}
	ids, err = probe.Running(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("expected empty runtime inventory for the probe, found %v, error %v", ids, err)
	}
}
