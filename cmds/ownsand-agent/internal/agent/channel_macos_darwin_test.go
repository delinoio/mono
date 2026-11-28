// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && arm64 && cgo

package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestNativeMacOSCapabilityPublication(t *testing.T) {
	oldSource := os.Getenv("OWNSAND_VZ_TEST_MACOS_IMAGE")
	newSource := os.Getenv("OWNSAND_VZ_TEST_MACOS_REPLACEMENT_IMAGE")
	if oldSource == "" || newSource == "" {
		t.Skip("set OWNSAND_VZ_TEST_MACOS_IMAGE and OWNSAND_VZ_TEST_MACOS_REPLACEMENT_IMAGE to complete images with different supported models; sign the test binary")
	}
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	dir := filepath.Join(t.TempDir(), images.MacOSDir)
	stage := images.BuildDir(dir)
	oldModel := cloneCapabilityImage(t, oldSource, dir)
	newModel := cloneCapabilityImage(t, newSource, stage)
	if bytes.Equal(oldModel, newModel) {
		t.Fatal("native acceptance needs two different hardware models supported by this Mac")
	}
	rt, err := vzrt.New(vzrt.Options{MacOSImage: dir, VMsDir: st.Path(store.VMsDir), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}

	const initialFree = 128 << 30
	var free atomic.Int64
	free.Store(initialFree)
	var deferred atomic.Int32
	h := startWith(t, cp, st, id, func(o *Options) {
		o.FreeDisk = func() (int64, error) { return free.Load(), nil }
		o.Runtime = capabilityProbeRuntime{Runtime: rt, probe: func(ctx context.Context) vmruntime.Capabilities {
			caps := rt.Capabilities(ctx)
			if caps.Deferred {
				deferred.Add(1)
			}
			return caps
		}}
	})
	isCapability := func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil }
	isState := func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }
	isHeartbeat := func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeat() != nil }
	first, initialCapability := h.wait(0, "initial native capability report", isCapability)
	assertMacOSCapabilityModel(t, first, oldModel)
	_, at := h.wait(initialCapability+1, "initial native state report", isState)
	var previous Status
	if err := st.LoadStatus(&previous); err != nil {
		t.Fatal(err)
	}

	release := pauseMacOSImagePublication(t, dir)
	free.Store(initialFree + capabilityDiskDelta + 1)
	waitForCapabilityDeferrals(t, &deferred, 2)
	_, at = h.wait(at+1, "heartbeat during native publication", isHeartbeat)
	var status Status
	if err := st.LoadStatus(&status); err != nil {
		t.Fatal(err)
	}
	if report, _ := cp.WaitFor(0, initialCapability+1, isCapability); report != nil ||
		!status.CapabilitiesAt.Equal(previous.CapabilitiesAt) ||
		!slices.Equal(status.BaseImages, previous.BaseImages) {
		t.Fatal("the agent did not retain native macOS capabilities during publication")
	}
	free.Store(initialFree)
	waitForCapabilityDeferrals(t, &deferred, deferred.Load()+2)
	if err := os.Rename(stage, dir); err != nil {
		t.Fatal(err)
	}
	release()
	replacement, at := h.wait(at+1, "replacement native capability report", isCapability)
	assertMacOSCapabilityModel(t, replacement, newModel)

	// Start another exchange, force a reconnect, and release the locks
	// without installing the staged generation to exercise backup recovery.
	if err := os.Rename(images.OldDir(dir), stage); err != nil {
		t.Fatal(err)
	}
	release = pauseMacOSImagePublication(t, dir)
	initialChannels, before := cp.Channels(), deferred.Load()
	cp.Current().Close()
	waitForCapabilityDeferrals(t, &deferred, before+2)
	if cp.Channels() != initialChannels {
		t.Fatal("native reconnect did not defer both initial reports")
	}
	release()
	restored, at := h.wait(at+1, "recovered native capability report", isCapability)
	assertMacOSCapabilityModel(t, restored, newModel)
	state, at := h.wait(at+1, "recovered native state report", isState)
	if !slices.Equal(state.GetStateReport().GetBuiltBaseImages(), restored.GetCapabilityReport().GetCapabilities().GetBuiltBaseImages()) {
		t.Fatal("initial reports disagree on the recovered image")
	}

	if err := os.Remove(filepath.Join(dir, images.MacOSAuxStorage)); err != nil {
		t.Fatal(err)
	}
	free.Store(initialFree + capabilityDiskDelta + 1)
	absent, _ := h.wait(at+1, "authoritative incomplete native image report", isCapability)
	caps := absent.GetCapabilityReport().GetCapabilities()
	if slices.Contains(caps.GetBuiltBaseImages(), ownsandv1.BaseImage_BASE_IMAGE_MACOS) || len(caps.GetMacosHardwareModels()) != 0 {
		t.Fatal("the native runtime advertised an incomplete image")
	}
}

func cloneCapabilityImage(t *testing.T, source, dest string) []byte {
	t.Helper()
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{images.MetadataFile, images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel} {
		if err := unix.Clonefile(filepath.Join(source, name), filepath.Join(dest, name), 0); err != nil {
			t.Fatalf("clone macOS image file %s (set TMPDIR to the images' APFS volume): %v", name, err)
		}
	}
	model, err := os.ReadFile(filepath.Join(dest, images.MacOSHardwareModel))
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func pauseMacOSImagePublication(t *testing.T, dir string) func() {
	t.Helper()
	build, err := images.TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.OpenFile(dir+".publication.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = build.Release()
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() {
		if err := errors.Join(pub.Close(), build.Release()); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(release)
	if err := unix.Flock(int(pub.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, images.OldDir(dir)); err != nil {
		t.Fatal(err)
	}
	return release
}
