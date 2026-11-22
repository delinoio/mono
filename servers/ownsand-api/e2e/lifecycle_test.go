// SPDX-License-Identifier: FSL-1.1-ALv2

// Package e2e runs the real OwnSand control plane, the real device agent
// with the process runtime, the Go SDK and the CLI binary together. It is
// enabled with OWNSAND_E2E=1 and OWNSAND_TEST_DATABASE_URL; see
// docs/project-ownsand.md#end-to-end-test.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// transports are the forced data path transports of the suite. Auto is not
// listed: on loopback it always succeeds peer-to-peer.
var transports = []ownsand.Transport{ownsand.TransportPeerToPeer, ownsand.TransportRelay}

func wantTransport(tr ownsand.Transport) ownsandv1.DataPathTransport {
	if tr == ownsand.TransportRelay {
		return ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY
	}
	return ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER
}

func createParams() ownsand.CreateSandboxParams {
	return ownsand.CreateSandboxParams{
		BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX,
		Resources: ownsand.Resources{VCPUCount: 1, MemoryMiB: 512, DiskGiB: 1},
		IdleTTL:   30 * time.Minute,
		MaxTTL:    2 * time.Hour,
		Network:   ownsandv1.NetworkMode_NETWORK_MODE_NONE,
	}
}

// execCollect runs a command and returns stdout, stderr and the result.
func execCollect(ctx context.Context, t *testing.T, sb *ownsand.Sandbox, cmd ...string) (string, string, *ownsand.ExecResult) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	res, err := sb.Exec(ctx, ownsand.ExecParams{Command: cmd, OnEvent: func(e ownsand.ExecEvent) error {
		switch e.Kind {
		case ownsand.ExecEventStdout:
			stdout.Write(e.Data)
		case ownsand.ExecEventStderr:
			stderr.Write(e.Data)
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("Exec %v: %v", cmd, err)
	}
	return stdout.String(), stderr.String(), res
}

// workDir returns the exec working directory of a sandbox. The process
// runtime gives no isolation: guest paths are host paths, and the sandbox
// root directory is the working directory, so files the suite writes must
// live below it (and the suite must not write to the host's /tmp).
func workDir(ctx context.Context, t *testing.T, sb *ownsand.Sandbox) string {
	t.Helper()
	out, _, res := execCollect(ctx, t, sb, "/bin/pwd")
	dir := strings.TrimSpace(out)
	if exitCode(res) != 0 || !strings.HasPrefix(dir, "/") {
		t.Fatalf("pwd = (%q, %+v)", out, res)
	}
	return strings.TrimSuffix(dir, "/")
}

func exitCode(r *ownsand.ExecResult) int64 {
	if r == nil || r.Kind != ownsand.ExecResultExited || r.ExitCode == nil {
		return -1
	}
	return *r.ExitCode
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitSnapshotReady(ctx context.Context, t *testing.T, c *ownsand.Client, id string) *ownsandv1.Snapshot {
	t.Helper()
	for {
		res, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: id})
		if err != nil {
			t.Fatalf("GetSnapshot: %v", err)
		}
		switch res.GetSnapshot().GetState() {
		case ownsandv1.SnapshotState_SNAPSHOT_STATE_READY:
			return res.GetSnapshot()
		case ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED:
			t.Fatalf("snapshot %s failed", id)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot %s did not become ready: %v", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitLocalSnapshotGone waits until the agent deleted its local copy of a
// snapshot.
func (s *stack) waitLocalSnapshotGone(t *testing.T, a agentProc, snapshotID string) {
	t.Helper()
	dir := filepath.Join(a.stateDir, "snapshots", snapshotID)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent kept its local copy of snapshot %s", snapshotID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLifecycleSDK drives the full sandbox lifecycle through the Go SDK on
// each forced transport.
func TestLifecycleSDK(t *testing.T) {
	s := newStack(t)
	for _, tr := range transports {
		t.Run(tr.String(), func(t *testing.T) { sdkLifecycle(t, s, tr) })
	}
}

// sdkLifecycle drives the full sandbox lifecycle through the Go SDK on one
// forced transport.
func sdkLifecycle(t *testing.T, s *stack, tr ownsand.Transport) {
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(tr)
	sb, err := c.CreateSandbox(ctx, createParams())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer sb.Close()
	info, err := sb.WaitStable(ctx)
	if err != nil {
		t.Fatalf("WaitStable: %v", err)
	}
	if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox state %v, want RUNNING", info.GetState())
	}

	stdout, stderr, res := execCollect(ctx, t, sb, "/bin/sh", "-c", "echo out; echo err >&2; exit 7")
	if stdout != "out\n" || stderr != "err\n" || exitCode(res) != 7 {
		t.Fatalf("exec = (%q, %q, %+v), want (out, err, 7)", stdout, stderr, res)
	}
	if res.Transport != wantTransport(tr) {
		t.Errorf("exec transport %v, want %v", res.Transport, wantTransport(tr))
	}

	// Multi-chunk file write and read (the SDK chunks at 1 MiB).
	root := workDir(ctx, t, sb)
	data := randomBytes(t, 3<<20+12345)
	n, err := sb.WriteFile(ctx, root+"/e2e.bin", bytes.NewReader(data), ownsand.WriteOptions{})
	if err != nil || n != int64(len(data)) {
		t.Fatalf("WriteFile = (%d, %v)", n, err)
	}
	got, err := sb.ReadFileBytes(ctx, root+"/e2e.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("ReadFile returned %d bytes (sha %x), want %d bytes (sha %x)", len(got), sha256.Sum256(got), len(data), sha256.Sum256(data))
	}
	out, _, res := execCollect(ctx, t, sb, "/bin/sh", "-c", "wc -c < e2e.bin")
	if strings.TrimSpace(out) != strconv.Itoa(len(data)) || exitCode(res) != 0 {
		t.Fatalf("guest size %q (%+v)", out, res)
	}

	testAttachExec(ctx, t, sb)

	if p, err := sb.Pause(ctx); err != nil || p.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("Pause = (%v, %v)", p.GetState(), err)
	}
	if r, err := sb.Resume(ctx); err != nil || r.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("Resume = (%v, %v)", r.GetState(), err)
	}
	if out, _, res := execCollect(ctx, t, sb, "/bin/echo", "resumed"); out != "resumed\n" || exitCode(res) != 0 {
		t.Fatalf("exec after resume = (%q, %+v)", out, res)
	}

	snap, err := sb.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	waitSnapshotReady(ctx, t, c, snap.GetSnapshotId())

	// A sandbox from the snapshot holds the written file.
	p := createParams()
	p.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	p.SnapshotID = snap.GetSnapshotId()
	restored, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot: %v", err)
	}
	defer restored.Close()
	if info, err := restored.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("restored WaitStable = (%v, %v)", info.GetState(), err)
	}
	// The restored sandbox has its own root; the file came with the
	// snapshot.
	got, err = restored.ReadFileBytes(ctx, workDir(ctx, t, restored)+"/e2e.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored ReadFile = (%d bytes, %v)", len(got), err)
	}
	if _, err := restored.Delete(ctx); err != nil {
		t.Fatalf("delete restored: %v", err)
	}

	if _, err := c.Snapshots().Delete(ctx, &ownsandv1.DeleteSnapshotRequest{SnapshotId: snap.GetSnapshotId()}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	s.waitLocalSnapshotGone(t, s.agents[0], snap.GetSnapshotId())
	if _, err := sb.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	final, err := sb.WaitStable(ctx)
	var ended *ownsand.ErrSandboxEnded
	if err != nil && !errors.As(err, &ended) {
		t.Fatalf("WaitStable after delete: %v", err)
	}
	if err == nil && final.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("state after delete %v", final.GetState())
	}
}

// testAttachExec starts an exec, drops its stream after the first output and
// attaches again with AttachExec from the received offset.
func testAttachExec(ctx context.Context, t *testing.T, sb *ownsand.Sandbox) {
	t.Helper()
	errDetach := errors.New("detach")
	var first bytes.Buffer
	var execID string
	_, err := sb.Exec(ctx, ownsand.ExecParams{
		Command: []string{"/bin/sh", "-c", "echo one; sleep 2; echo two; exit 3"},
		OnEvent: func(e ownsand.ExecEvent) error {
			if e.ExecID != "" {
				execID = e.ExecID
			}
			if e.Kind == ownsand.ExecEventStdout {
				first.Write(e.Data)
				return errDetach
			}
			return nil
		},
	})
	if !errors.Is(err, errDetach) {
		t.Fatalf("Exec detach error = %v", err)
	}
	if execID == "" || first.String() != "one\n" {
		t.Fatalf("first output %q exec %q", first.String(), execID)
	}
	var rest bytes.Buffer
	res, err := sb.AttachExec(ctx, execID, ownsand.AttachParams{StdoutOffset: int64(first.Len()), OnEvent: func(e ownsand.ExecEvent) error {
		if e.Kind == ownsand.ExecEventStdout {
			rest.Write(e.Data)
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("AttachExec: %v", err)
	}
	if rest.String() != "two\n" || exitCode(res) != 3 {
		t.Fatalf("AttachExec = (%q, %+v), want (two, 3)", rest.String(), res)
	}
}

// TestLifecycleCLI drives the lifecycle through the CLI binary on each
// forced transport.
func TestLifecycleCLI(t *testing.T) {
	s := newStack(t)
	for _, tr := range transports {
		t.Run(tr.String(), func(t *testing.T) { cliLifecycle(t, s, tr) })
	}
}

// cliLifecycle drives the sandbox lifecycle through the CLI binary on one
// forced transport.
func cliLifecycle(t *testing.T, s *stack, tr ownsand.Transport) {
	r := s.cli(tr, nil, "sandbox", "create", "--image", "linux", "--idle-ttl", "30m", "--max-ttl", "2h",
		"--vcpu", "1", "--memory", "512", "--disk", "1", "--network", "none", "--format", "json")
	if r.code != 0 {
		t.Fatalf("sandbox create exit %d: %s", r.code, r.stderr)
	}
	var created struct {
		SandboxID string `json:"sandbox_id"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(r.stdout, &created); err != nil {
		t.Fatalf("sandbox create output %q: %v", r.stdout, err)
	}
	if created.State != "SANDBOX_STATE_RUNNING" {
		t.Fatalf("created state %q", created.State)
	}
	id := created.SandboxID
	r = s.cli(tr, nil, "sandbox", "exec", id, "--", "/bin/pwd")
	// Check the absolute path before the trailing slash is trimmed: the
	// working directory of a VM sandbox is "/", which trims to "".
	root := strings.TrimSpace(string(r.stdout))
	if r.code != 0 || !strings.HasPrefix(root, "/") {
		t.Fatalf("pwd = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}
	root = strings.TrimSuffix(root, "/")

	r = s.cli(tr, nil, "sandbox", "exec", id, "--", "/bin/sh", "-c", "echo hi; echo oops >&2; exit 5")
	if r.code != 5 || string(r.stdout) != "hi\n" || !strings.Contains(r.stderr, "oops\n") {
		t.Fatalf("sandbox exec = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}
	r = s.cli(tr, nil, "sandbox", "exec", "--format", "json", id, "--", "/bin/echo", "json")
	if r.code != 0 || !bytes.Contains(r.stdout, []byte(`"exit_code"`)) || !bytes.Contains(r.stdout, []byte(`"transport":"`+tr.String()+`"`)) {
		t.Fatalf("sandbox exec json = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}

	data := randomBytes(t, 2<<20+777)
	local := filepath.Join(t.TempDir(), "local.bin")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if r = s.cli(tr, nil, "file", "write", id, root+"/cli.bin", "--from", local); r.code != 0 {
		t.Fatalf("file write = (%d, %q)", r.code, r.stderr)
	}
	r = s.cli(tr, nil, "file", "read", id, root+"/cli.bin")
	if r.code != 0 || !bytes.Equal(r.stdout, data) {
		t.Fatalf("file read = (%d, %d bytes, %q)", r.code, len(r.stdout), r.stderr)
	}
	if r = s.cli(tr, strings.NewReader("stdin-data"), "file", "write", id, root+"/stdin.txt"); r.code != 0 {
		t.Fatalf("file write stdin = (%d, %q)", r.code, r.stderr)
	}
	if r = s.cli(tr, nil, "file", "read", id, root+"/stdin.txt"); r.code != 0 || string(r.stdout) != "stdin-data" {
		t.Fatalf("file read stdin = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}

	for _, cmd := range []string{"pause", "resume"} {
		if r = s.cli(tr, nil, "sandbox", cmd, id, "--format", "json"); r.code != 0 {
			t.Fatalf("sandbox %s = (%d, %q)", cmd, r.code, r.stderr)
		}
	}
	r = s.cli(tr, nil, "snapshot", "create", id, "--format", "json")
	if r.code != 0 {
		t.Fatalf("snapshot create = (%d, %q)", r.code, r.stderr)
	}
	var snap struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(r.stdout, &snap); err != nil || snap.SnapshotID == "" {
		t.Fatalf("snapshot create output %q: %v", r.stdout, err)
	}
	ctx := s.ctx(2 * time.Minute)
	waitSnapshotReady(ctx, t, s.apiKeyClient(tr), snap.SnapshotID)
	if r = s.cli(tr, nil, "snapshot", "delete", snap.SnapshotID); r.code != 0 {
		t.Fatalf("snapshot delete = (%d, %q)", r.code, r.stderr)
	}
	s.waitLocalSnapshotGone(t, s.agents[0], snap.SnapshotID)
	if r = s.cli(tr, nil, "sandbox", "delete", id); r.code != 0 {
		t.Fatalf("sandbox delete = (%d, %q)", r.code, r.stderr)
	}
}
