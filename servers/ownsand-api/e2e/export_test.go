// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func waitExported(ctx context.Context, t *testing.T, c *ownsand.Client, id string) *ownsandv1.Snapshot {
	t.Helper()
	for {
		res, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: id})
		if err != nil {
			t.Fatalf("GetSnapshot: %v", err)
		}
		switch res.GetSnapshot().GetExportState() {
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED:
			return res.GetSnapshot()
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED:
			t.Fatalf("snapshot %s export failed", id)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot %s was not exported: %v", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// exportAttempts returns the exported parts of a snapshot by attempt ID: the
// size of each object by its part number.
func (s *stack) exportAttempts(snapID string) map[string]map[int]int {
	attempts := map[string]map[int]int{}
	for k, size := range s.objects.sizes("organizations/" + s.orgID + "/snapshots/" + snapID + "/attempts/") {
		_, rest, _ := strings.Cut(k, "/attempts/")
		attempt, part, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(part)
		if err != nil {
			n = -1
		}
		if attempts[attempt] == nil {
			attempts[attempt] = map[int]int{}
		}
		attempts[attempt][n] = size
	}
	return attempts
}

// checkExportParts checks the parts of one complete export attempt: they are
// numbered 1 to n with n of at least 2 (wantParts exactly, when it is not
// zero), every part except the last has the part size and the last part
// holds 1 to partSize bytes.
func checkExportParts(parts map[int]int, partSize, wantParts int) error {
	n := len(parts)
	if wantParts != 0 && n != wantParts {
		return fmt.Errorf("%d parts, want %d", n, wantParts)
	}
	if n < 2 {
		return fmt.Errorf("%d parts, want at least 2", n)
	}
	for i := 1; i <= n; i++ {
		size, ok := parts[i]
		switch {
		case !ok:
			return fmt.Errorf("part %d is missing from parts 1 to %d", i, n)
		case i < n && size != partSize:
			return fmt.Errorf("part %d has %d bytes, want the part size %d", i, size, partSize)
		case i == n && (size < 1 || size > partSize):
			return fmt.Errorf("last part %d has %d bytes, want 1 to %d", i, size, partSize)
		}
	}
	return nil
}

// wantExportParts is the exact number of parts of the 12 MiB snapshot of the
// export tests, or zero when the runtime makes it unknown: a process runtime
// snapshot holds only the sandbox root, which makes a tar stream of three 5
// MiB parts, and a VM snapshot also holds the disk image and the memory
// state.
func (s *stack) wantExportParts() int {
	if s.processRuntime {
		return 3
	}
	return 0
}

// TestSnapshotExportImport exports a snapshot from one device to the fake
// S3 store in several parts, creates a sandbox from it on a second device
// (which imports it with the hashes from the control plane) and deletes the
// snapshot, which removes both local copies.
func TestSnapshotExportImport(t *testing.T) {
	s := newStack(t, stackOptions{agents: 2, objects: true})
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	a, b := s.agents[0], s.agents[1]

	p := createParams()
	p.DeviceID = a.deviceID
	src, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer src.Close()
	if info, err := src.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = (%v, %v)", info.GetState(), err)
	}
	// 12 MiB of random data makes a tar stream of three 5 MiB parts with
	// the process runtime (see wantExportParts).
	data := randomBytes(t, 12<<20)
	if _, err := src.WriteFile(ctx, workDir(ctx, t, src)+"/data.bin", bytes.NewReader(data), ownsand.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snapID := snap.GetSnapshotId()
	waitSnapshotReady(ctx, t, c, snapID)
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	waitExported(ctx, t, c, snapID)
	// Each export attempt writes its parts under its own prefix.
	attempts := s.exportAttempts(snapID)
	if len(attempts) != 1 {
		t.Fatalf("exported attempts %v, want one", attempts)
	}
	for id, parts := range attempts {
		if err := checkExportParts(parts, e2ePartSize, s.wantExportParts()); err != nil {
			t.Fatalf("export attempt %s: %v", id, err)
		}
	}

	p = createParams()
	p.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	p.SnapshotID = snapID
	p.DeviceID = b.deviceID
	restored, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox on the second device: %v", err)
	}
	defer restored.Close()
	info, err := restored.WaitStable(ctx)
	if err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || info.GetDeviceId() != b.deviceID {
		t.Fatalf("restored WaitStable = (%v on %s, %v)", info.GetState(), info.GetDeviceId(), err)
	}
	got, err := restored.ReadFileBytes(ctx, workDir(ctx, t, restored)+"/data.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored ReadFile = (%d bytes, %v)", len(got), err)
	}
	for _, sb := range []*ownsand.Sandbox{restored, src} {
		if _, err := sb.Delete(ctx); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if _, err := c.Snapshots().Delete(ctx, &ownsandv1.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	s.waitLocalSnapshotGone(t, a, snapID)
	s.waitLocalSnapshotGone(t, b, snapID)
}

// TestCheckExportParts checks the part rules of an export attempt, which the
// export tests apply to every runtime: a VM snapshot has many more parts
// than the three of the process runtime.
func TestCheckExportParts(t *testing.T) {
	const size = 10
	for _, tc := range []struct {
		name  string
		parts map[int]int
		want  int
		ok    bool
	}{
		{name: "three parts", parts: map[int]int{1: size, 2: size, 3: 4}, want: 3, ok: true},
		{name: "full last part", parts: map[int]int{1: size, 2: size}, ok: true},
		{name: "many parts of a VM snapshot", parts: map[int]int{1: size, 2: size, 3: size, 4: size, 5: 1}, ok: true},
		{name: "count differs from the wanted count", parts: map[int]int{1: size, 2: size, 3: size, 4: 1}, want: 3},
		{name: "one part", parts: map[int]int{1: 4}},
		{name: "gap", parts: map[int]int{1: size, 3: 4}},
		{name: "unparsable part number", parts: map[int]int{1: size, -1: 4}},
		{name: "short middle part", parts: map[int]int{1: size, 2: 9, 3: 4}},
		{name: "empty last part", parts: map[int]int{1: size, 2: 0}},
		{name: "oversized last part", parts: map[int]int{1: size, 2: size + 1}},
	} {
		if err := checkExportParts(tc.parts, size, tc.want); (err == nil) != tc.ok {
			t.Errorf("%s: checkExportParts(%v) = %v, want ok %v", tc.name, tc.parts, err, tc.ok)
		}
	}
}
