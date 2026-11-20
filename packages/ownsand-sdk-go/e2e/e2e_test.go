// Package e2e is the opt-in end-to-end acceptance test of OwnSand. It builds
// and starts the real control plane (ownsand-api), the real device agent
// (ownsand-agent, with Firecracker microVMs) and the ownsand CLI, and drives
// them with the Go SDK. It imports only public packages; the control plane
// is used only as a built binary.
//
// Run it on Linux with /dev/kvm; docs/project-ownsand.md (End-to-end
// acceptance) lists the artifacts in OWNSAND_E2E_FC_DIR:
//
//	OWNSAND_E2E=1 OWNSAND_E2E_DATABASE_URL=postgres://... OWNSAND_E2E_FC_DIR=<dir> \
//	  go test -v -timeout 30m ./packages/ownsand-sdk-go/e2e/
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// results collects the outcome of every scenario step for the summary.
type results struct {
	t    *testing.T
	rows []string
}

// step runs one scenario step and records it. A failed step marks the test
// failed but lets independent later steps run.
func (r *results) step(name string, f func() error) bool {
	start := time.Now()
	err := f()
	status := "PASS"
	if err != nil {
		status = "FAIL: " + err.Error()
		r.t.Errorf("%s: %v", name, err)
	}
	r.rows = append(r.rows, fmt.Sprintf("%-60s %-8s %s", name, time.Since(start).Round(100*time.Millisecond), status))
	return err == nil
}

func (r *results) summary() {
	r.t.Logf("scenario results:\n%s", strings.Join(r.rows, "\n"))
}

func TestOwnSandEndToEnd(t *testing.T) {
	h := newHarness(t)
	res := &results{t: t}
	defer res.summary()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	h.startAPI()
	ownerSub := "e2e-owner"
	user, err := ownsand.New(ownsand.Options{
		Origin: h.origin,
		Token:  func(context.Context) (string, error) { return h.issuer.token(ownerSub), nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	var org *ownsandv1.Organization
	if !res.step("user creates an organization", func() error {
		org, err = user.CreateOrganization(ctx, "e2e")
		return err
	}) {
		return
	}
	user, _ = ownsand.New(ownsand.Options{
		Origin:         h.origin,
		OrganizationID: org.GetOrganizationId(),
		Token:          func(context.Context) (string, error) { return h.issuer.token(ownerSub), nil },
	})

	stateDir := h.agentStateDir()
	var deviceID string
	if !res.step("admin creates an enrollment token and the agent enrolls", func() error {
		_, secret, err := user.CreateEnrollmentToken(ctx, time.Hour, "e2e-device")
		if err != nil {
			return err
		}
		tokenFile := filepath.Join(stateDir, "token")
		if err := os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600); err != nil {
			return err
		}
		defer os.Remove(tokenFile)
		p := h.start("agent-enroll", h.agentEnv(), "--state-dir", stateDir, "enroll", "--origin", h.origin, "--token-file", tokenFile)
		select {
		case <-p.done:
		case <-time.After(2 * time.Minute):
			return errors.New("enroll did not finish")
		}
		if code := p.cmd.ProcessState.ExitCode(); code != 0 {
			return fmt.Errorf("enroll exited with %d: %s", code, tail(filepath.Join(h.logDir, "agent-enroll.log"), 5))
		}
		devs, _, err := user.ListDevices(ctx, ownsand.ListOptions{}, false)
		if err != nil {
			return err
		}
		if len(devs) != 1 {
			return fmt.Errorf("%d devices after enrollment", len(devs))
		}
		deviceID = devs[0].GetDeviceId()
		return nil
	}) {
		return
	}

	agent := h.start("agent", h.agentEnv(), "--state-dir", stateDir, "run")
	if !res.step("the agent connects and the device is ONLINE", func() error {
		return waitDevice(ctx, user, deviceID, ownsandv1.DeviceState_DEVICE_STATE_ONLINE, 90*time.Second)
	}) {
		return
	}
	res.step("admin sets the device limits", func() error {
		d, err := user.UpdateDeviceLimits(ctx, deviceID, &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 2048, DiskGib: 20})
		if err != nil {
			return err
		}
		if d.GetLimits().GetVcpuCount() != 4 {
			return fmt.Errorf("limits not applied: %v", d.GetLimits())
		}
		return nil
	})

	for _, tr := range []struct {
		name string
		mode ownsand.TransportMode
		want ownsandv1.DataPathTransport
	}{
		{"relay", ownsand.TransportRelay, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY},
		{"peer-to-peer", ownsand.TransportPeerToPeer, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER},
	} {
		c, err := ownsand.New(ownsand.Options{
			Origin:         h.origin,
			OrganizationID: org.GetOrganizationId(),
			Token:          func(context.Context) (string, error) { return h.issuer.token(ownerSub), nil },
			Transport:      tr.mode,
		})
		if err != nil {
			t.Fatal(err)
		}
		sdkLifecycle(ctx, res, c, "sdk/"+tr.name, tr.want)
		sdkEdgeCases(ctx, res, user, c, "sdk/"+tr.name, tr.mode)
	}

	var apiKey string
	if res.step("admin creates an API key", func() error {
		_, apiKey, err = user.CreateAPIKey(ctx, "e2e-cli")
		return err
	}) {
		cliLifecycle(ctx, h, res, org.GetOrganizationId(), apiKey)
	}

	if os.Getenv(envLong) == "1" {
		res.step("relay: a silent 15-minute exec survives the 14-minute stream rotation", func() error {
			c, err := ownsand.New(ownsand.Options{
				Origin:         h.origin,
				OrganizationID: org.GetOrganizationId(),
				Token:          func(context.Context) (string, error) { return h.issuer.token(ownerSub), nil },
				Transport:      ownsand.TransportRelay,
			})
			if err != nil {
				return err
			}
			sb, err := createRunning(ctx, c, 30*time.Minute, time.Hour, "")
			if sb != nil {
				defer func() {
					_, _ = sb.Delete(ctx)
					sb.Close()
				}()
			}
			if err != nil {
				return err
			}
			ectx, cancel := context.WithTimeout(ctx, 20*time.Minute)
			defer cancel()
			st, err := sb.Exec(ectx, ownsand.ExecOptions{Command: []string{"/bin/sh", "-c", "sleep 900; echo done"}})
			if err != nil {
				return err
			}
			var stdout bytes.Buffer
			for {
				ev, err := st.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				if ev.Kind == ownsand.ExecEventStdout {
					stdout.Write(ev.Data)
				}
			}
			r, err := st.Result()
			if err != nil {
				return err
			}
			if stdout.String() != "done\n" || r.ExitCode == nil || *r.ExitCode != 0 || r.Incomplete {
				return fmt.Errorf("stdout %q result %+v", stdout.String(), r)
			}
			return nil
		})
	}

	res.step("TTL: an idle sandbox ends with IDLE_TTL_EXPIRED", func() error {
		sb, err := createRunning(ctx, user, 20*time.Second, time.Hour, "")
		if err != nil {
			return err
		}
		info, err := waitEnded(ctx, sb, 2*time.Minute)
		if err != nil {
			return err
		}
		if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || info.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
			return fmt.Errorf("sandbox ended %v %v", info.GetState(), info.GetEndReason())
		}
		return nil
	})

	res.step("agent restart: a running sandbox becomes FAILED LOST_ON_DEVICE", func() error {
		sb, err := createRunning(ctx, user, 10*time.Minute, time.Hour, "")
		if err != nil {
			return err
		}
		agent.kill()
		agent = h.start("agent-2", h.agentEnv(), "--state-dir", stateDir, "run")
		info, err := waitEnded(ctx, sb, 3*time.Minute)
		if err != nil {
			return err
		}
		if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || info.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
			return fmt.Errorf("sandbox ended %v %v", info.GetState(), info.GetEndReason())
		}
		return waitDevice(ctx, user, deviceID, ownsandv1.DeviceState_DEVICE_STATE_ONLINE, 90*time.Second)
	})

	res.step("device removal: sandboxes end with DEVICE_REMOVED and the agent cleans up", func() error {
		sb, err := createRunning(ctx, user, 10*time.Minute, time.Hour, "")
		if err != nil {
			return err
		}
		if err := user.RemoveDevice(ctx, deviceID); err != nil {
			return err
		}
		info, err := waitEnded(ctx, sb, time.Minute)
		if err != nil {
			return err
		}
		if info.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED {
			return fmt.Errorf("sandbox ended %v %v", info.GetState(), info.GetEndReason())
		}
		select {
		case <-agent.done:
		case <-time.After(2 * time.Minute):
			return errors.New("the agent did not exit after the removal")
		}
		if _, err := os.Stat(filepath.Join(stateDir, "identity.json")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the device identity is still on disk (%v)", err)
		}
		left, _ := filepath.Glob(filepath.Join(stateDir, "vms", "*"))
		if len(left) > 0 {
			return fmt.Errorf("%d VM directories are left", len(left))
		}
		devs, _, err := user.ListDevices(ctx, ownsand.ListOptions{}, true)
		if err != nil {
			return err
		}
		for _, d := range devs {
			if d.GetDeviceId() == deviceID && d.GetState() != ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
				return fmt.Errorf("device state %v", d.GetState())
			}
		}
		return nil
	})
}

func waitDevice(ctx context.Context, c *ownsand.Client, id string, want ownsandv1.DeviceState, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last ownsandv1.DeviceState
	for time.Now().Before(deadline) {
		d, err := c.GetDevice(ctx, id)
		if err != nil {
			return err
		}
		last = d.GetState()
		if last == want {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("device state %v, want %v", last, want)
}

func createOpts(idle, max time.Duration, snapshotID string) ownsand.CreateSandboxOptions {
	o := ownsand.CreateSandboxOptions{
		VCPUCount: 1, MemoryMiB: 256, DiskGiB: 1,
		IdleTTL: idle, MaxTTL: max,
		Network: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
	}
	if snapshotID != "" {
		o.SnapshotID = snapshotID
	} else {
		o.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_LINUX
	}
	return o
}

func createRunning(ctx context.Context, c *ownsand.Client, idle, max time.Duration, snapshotID string) (*ownsand.Sandbox, error) {
	sb, err := c.CreateSandbox(ctx, createOpts(idle, max, snapshotID))
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	info, err := sb.Wait(wctx, 500*time.Millisecond)
	if err != nil {
		return sb, err
	}
	if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return sb, fmt.Errorf("sandbox is %v (%v), want RUNNING", info.GetState(), info.GetEndReason())
	}
	return sb, nil
}

func waitEnded(ctx context.Context, sb *ownsand.Sandbox, timeout time.Duration) (*ownsandv1.Sandbox, error) {
	deadline := time.Now().Add(timeout)
	var info *ownsandv1.Sandbox
	var err error
	for time.Now().Before(deadline) {
		info, err = sb.Refresh(ctx)
		if err != nil {
			return nil, err
		}
		switch info.GetState() {
		case ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxState_SANDBOX_STATE_FAILED:
			return info, nil
		}
		time.Sleep(time.Second)
	}
	return info, fmt.Errorf("sandbox still %v after %v", info.GetState(), timeout)
}

func waitState(ctx context.Context, sb *ownsand.Sandbox, want ownsandv1.SandboxState, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last ownsandv1.SandboxState
	for time.Now().Before(deadline) {
		info, err := sb.Refresh(ctx)
		if err != nil {
			return err
		}
		last = info.GetState()
		if last == want {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sandbox state %v, want %v", last, want)
}

func waitSnapshotReady(ctx context.Context, c *ownsand.Client, id string) error {
	deadline := time.Now().Add(2 * time.Minute)
	var last ownsandv1.SnapshotState
	for time.Now().Before(deadline) {
		s, err := c.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		last = s.GetState()
		switch last {
		case ownsandv1.SnapshotState_SNAPSHOT_STATE_READY:
			return nil
		case ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED:
			return errors.New("snapshot FAILED")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("snapshot state %v", last)
}

type execOutput struct {
	stdout, stderr bytes.Buffer
	result         *ownsand.ExecResult
}

func runExec(ctx context.Context, sb *ownsand.Sandbox, command ...string) (*execOutput, error) {
	ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := sb.Exec(ectx, ownsand.ExecOptions{Command: command})
	if err != nil {
		return nil, err
	}
	out := &execOutput{}
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		switch ev.Kind {
		case ownsand.ExecEventStdout:
			out.stdout.Write(ev.Data)
		case ownsand.ExecEventStderr:
			out.stderr.Write(ev.Data)
		}
	}
	out.result, err = st.Result()
	return out, err
}

// sdkLifecycle runs the project success criterion through the SDK on one
// forced transport.
func sdkLifecycle(ctx context.Context, res *results, c *ownsand.Client, prefix string, transport ownsandv1.DataPathTransport) {
	var sb *ownsand.Sandbox
	if !res.step(prefix+": create sandbox (RUNNING)", func() error {
		var err error
		sb, err = createRunning(ctx, c, 10*time.Minute, time.Hour, "")
		return err
	}) {
		return
	}
	defer sb.Close()
	res.step(prefix+": exec (stdout, stderr, exit code)", func() error {
		out, err := runExec(ctx, sb, "/bin/sh", "-c", "echo out; echo err >&2; exit 3")
		if err != nil {
			return err
		}
		r := out.result
		if out.stdout.String() != "out\n" || out.stderr.String() != "err\n" {
			return fmt.Errorf("stdout %q stderr %q", out.stdout.String(), out.stderr.String())
		}
		if r.Outcome != ownsand.ExecExited || r.ExitCode == nil || *r.ExitCode != 3 {
			return fmt.Errorf("result %+v", r)
		}
		if r.Transport != transport {
			return fmt.Errorf("transport %v, want %v", r.Transport, transport)
		}
		return nil
	})
	payload := make([]byte, 2*1024*1024+12345)
	_, _ = rand.Read(payload)
	res.step(prefix+": write file (multi-chunk)", func() error {
		return sb.WriteFileBytes(ctx, "/root/payload.bin", payload, 0o600)
	})
	res.step(prefix+": read file", func() error {
		got, err := sb.ReadFileBytes(ctx, "/root/payload.bin")
		if err != nil {
			return err
		}
		if !bytes.Equal(got, payload) {
			return fmt.Errorf("read %d bytes that differ from the %d written", len(got), len(payload))
		}
		return nil
	})
	res.step(prefix+": write and read an empty file", func() error {
		if err := sb.WriteFileBytes(ctx, "/root/empty", nil, 0o644); err != nil {
			return err
		}
		got, err := sb.ReadFileBytes(ctx, "/root/empty")
		if err != nil {
			return err
		}
		if len(got) != 0 {
			return fmt.Errorf("read %d bytes", len(got))
		}
		return nil
	})
	res.step(prefix+": pause", func() error {
		info, err := sb.Pause(ctx)
		if err != nil {
			return err
		}
		if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			return waitState(ctx, sb, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, 30*time.Second)
		}
		return nil
	})
	res.step(prefix+": resume and exec again", func() error {
		info, err := sb.Resume(ctx)
		if err != nil {
			return err
		}
		if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			if err := waitState(ctx, sb, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 30*time.Second); err != nil {
				return err
			}
		}
		out, err := runExec(ctx, sb, "/bin/cat", "/proc/uptime")
		if err != nil {
			return err
		}
		if out.result.ExitCode == nil || *out.result.ExitCode != 0 || out.stdout.Len() == 0 {
			return fmt.Errorf("result %+v stdout %d bytes", out.result, out.stdout.Len())
		}
		return nil
	})
	var snapID string
	res.step(prefix+": snapshot (READY)", func() error {
		s, err := sb.Snapshot(ctx)
		if err != nil {
			return err
		}
		snapID = s.GetSnapshotId()
		return waitSnapshotReady(ctx, c, snapID)
	})
	if snapID != "" {
		res.step(prefix+": create a sandbox from the snapshot and read its disk", func() error {
			sb2, err := createRunning(ctx, c, 10*time.Minute, time.Hour, snapID)
			if sb2 != nil {
				defer func() {
					_, _ = sb2.Delete(ctx)
					sb2.Close()
				}()
			}
			if err != nil {
				return err
			}
			got, err := sb2.ReadFileBytes(ctx, "/root/payload.bin")
			if err != nil {
				return err
			}
			if !bytes.Equal(got, payload) {
				return errors.New("the restored sandbox has other file contents")
			}
			out, err := runExec(ctx, sb2, "/bin/echo", "restored")
			if err != nil {
				return err
			}
			if out.stdout.String() != "restored\n" {
				return fmt.Errorf("stdout %q", out.stdout.String())
			}
			_, err = sb2.Delete(ctx)
			if err != nil {
				return err
			}
			return waitState(ctx, sb2, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, time.Minute)
		})
	}
	res.step(prefix+": delete", func() error {
		info, err := sb.Delete(ctx)
		if err != nil {
			return err
		}
		if info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			if err := waitState(ctx, sb, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, time.Minute); err != nil {
				return err
			}
		}
		info, _ = sb.Refresh(ctx)
		if info.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
			return fmt.Errorf("end reason %v", info.GetEndReason())
		}
		if snapID != "" {
			return c.DeleteSnapshot(ctx, snapID)
		}
		return nil
	})
}

// cliLifecycle runs the scenario once through the ownsand CLI with an API key.
func cliLifecycle(ctx context.Context, h *harness, res *results, orgID, apiKey string) {
	env := []string{"OWNSAND_API_KEY=" + apiKey, "OWNSAND_ORG=" + orgID, "OWNSAND_API_ORIGIN=" + h.origin, "NO_COLOR=1",
		"OWNSAND_CONFIG_DIR=" + h.t.TempDir()}
	cli := func(stdin []byte, args ...string) (string, string, int) {
		return h.runCLI(env, stdin, args...)
	}
	jsonField := func(out, field string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			return ""
		}
		s, _ := m[field].(string)
		return s
	}
	waitCLIState := func(id, want string) error {
		deadline := time.Now().Add(2 * time.Minute)
		var last string
		for time.Now().Before(deadline) {
			out, errOut, code := cli(nil, "sandbox", "get", id, "--format", "json")
			if code != 0 {
				return fmt.Errorf("sandbox get exited %d: %s", code, errOut)
			}
			last = jsonField(out, "state")
			if last == want {
				return nil
			}
			if last == "SANDBOX_STATE_FAILED" || last == "SANDBOX_STATE_DELETED" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		return fmt.Errorf("state %s, want %s", last, want)
	}
	create := func(source ...string) (string, error) {
		args := append([]string{"sandbox", "create", "--format", "json", "--idle-ttl", "10m", "--max-ttl", "1h",
			"--vcpu", "1", "--memory", "256", "--disk", "1", "--network", "none"}, source...)
		out, errOut, code := cli(nil, args...)
		if code != 0 {
			return "", fmt.Errorf("sandbox create exited %d: %s", code, errOut)
		}
		id := jsonField(out, "sandbox_id")
		if id == "" {
			return "", fmt.Errorf("no sandbox_id in %q", out)
		}
		return id, waitCLIState(id, "SANDBOX_STATE_RUNNING")
	}
	for _, transport := range []string{"relay", "peer-to-peer"} {
		prefix := "cli/" + transport
		tflag := "--transport=" + transport
		var id string
		if !res.step(prefix+": sandbox create", func() error {
			var err error
			id, err = create("--image", "linux")
			return err
		}) {
			continue
		}
		res.step(prefix+": sandbox exec (exit code propagates)", func() error {
			out, errOut, code := cli(nil, "sandbox", "exec", tflag, id, "--", "/bin/sh", "-c", "echo cli-out; echo cli-err >&2; exit 7")
			if code != 7 || out != "cli-out\n" || !strings.Contains(errOut, "cli-err\n") {
				return fmt.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
			}
			return nil
		})
		data := []byte("written by the ownsand CLI\n")
		res.step(prefix+": file write and read", func() error {
			if _, errOut, code := cli(data, "file", "write", tflag, id, "/root/cli.txt"); code != 0 {
				return fmt.Errorf("file write exited %d: %s", code, errOut)
			}
			out, errOut, code := cli(nil, "file", "read", tflag, id, "/root/cli.txt")
			if code != 0 || out != string(data) {
				return fmt.Errorf("file read exited %d (%s), got %q", code, errOut, out)
			}
			return nil
		})
		res.step(prefix+": pause and resume", func() error {
			if _, errOut, code := cli(nil, "sandbox", "pause", id); code != 0 {
				return fmt.Errorf("pause exited %d: %s", code, errOut)
			}
			if err := waitCLIState(id, "SANDBOX_STATE_PAUSED"); err != nil {
				return err
			}
			if _, errOut, code := cli(nil, "sandbox", "resume", id); code != 0 {
				return fmt.Errorf("resume exited %d: %s", code, errOut)
			}
			return waitCLIState(id, "SANDBOX_STATE_RUNNING")
		})
		var snapID string
		res.step(prefix+": snapshot create", func() error {
			out, errOut, code := cli(nil, "snapshot", "create", id, "--format", "json")
			if code != 0 {
				return fmt.Errorf("snapshot create exited %d: %s", code, errOut)
			}
			snapID = jsonField(out, "snapshot_id")
			deadline := time.Now().Add(2 * time.Minute)
			for time.Now().Before(deadline) {
				out, _, _ := cli(nil, "snapshot", "get", snapID, "--format", "json")
				if st := jsonField(out, "state"); st == "SNAPSHOT_STATE_READY" {
					return nil
				} else if st == "SNAPSHOT_STATE_FAILED" {
					return errors.New("snapshot FAILED")
				}
				time.Sleep(500 * time.Millisecond)
			}
			return errors.New("snapshot not READY")
		})
		if snapID != "" {
			res.step(prefix+": sandbox create from snapshot", func() error {
				id2, err := create("--snapshot", snapID)
				if id2 != "" {
					defer cli(nil, "sandbox", "delete", id2)
				}
				if err != nil {
					return err
				}
				out, errOut, code := cli(nil, "file", "read", tflag, id2, "/root/cli.txt")
				if code != 0 || out != string(data) {
					return fmt.Errorf("file read exited %d (%s), got %q", code, errOut, out)
				}
				return nil
			})
		}
		res.step(prefix+": sandbox delete", func() error {
			if _, errOut, code := cli(nil, "sandbox", "delete", id); code != 0 {
				return fmt.Errorf("delete exited %d: %s", code, errOut)
			}
			if err := waitCLIState(id, "SANDBOX_STATE_DELETED"); err != nil {
				return err
			}
			if snapID != "" {
				if _, errOut, code := cli(nil, "snapshot", "delete", snapID); code != 0 {
					return fmt.Errorf("snapshot delete exited %d: %s", code, errOut)
				}
			}
			return nil
		})
	}
}

// sdkEdgeCases checks the error frames, exec results other than an exit
// code, AttachExec and revocation on one forced transport.
func sdkEdgeCases(ctx context.Context, res *results, admin, c *ownsand.Client, prefix string, mode ownsand.TransportMode) {
	var sb *ownsand.Sandbox
	if !res.step(prefix+": edge cases: create sandbox", func() error {
		var err error
		sb, err = createRunning(ctx, c, 10*time.Minute, time.Hour, "")
		return err
	}) {
		return
	}
	defer func() {
		_, _ = sb.Delete(ctx)
		sb.Close()
	}()
	res.step(prefix+": exec of a missing program is a failed result", func() error {
		out, err := runExec(ctx, sb, "/no/such/program")
		if err != nil {
			return err
		}
		if out.result.Outcome != ownsand.ExecFailed || out.result.FailureReason == "" {
			return fmt.Errorf("result %+v", out.result)
		}
		return nil
	})
	res.step(prefix+": exec timeout sets timed_out", func() error {
		ectx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		st, err := sb.Exec(ectx, ownsand.ExecOptions{Command: []string{"/bin/sleep", "30"}, Timeout: time.Second})
		if err != nil {
			return err
		}
		r, err := st.Wait()
		if err != nil {
			return err
		}
		if r.Outcome != ownsand.ExecExited || !r.TimedOut {
			return fmt.Errorf("result %+v", r)
		}
		return nil
	})
	res.step(prefix+": AttachExec returns the kept output and result", func() error {
		st, err := sb.Exec(ctx, ownsand.ExecOptions{Command: []string{"/bin/sh", "-c", "echo kept; exit 5"}})
		if err != nil {
			return err
		}
		if _, err := st.Wait(); err != nil {
			return err
		}
		at, err := sb.AttachExec(ctx, st.ExecID())
		if err != nil {
			return err
		}
		var stdout bytes.Buffer
		for {
			ev, err := at.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if ev.Kind == ownsand.ExecEventStdout {
				stdout.Write(ev.Data)
			}
		}
		r, err := at.Result()
		if err != nil {
			return err
		}
		if stdout.String() != "kept\n" || r.ExitCode == nil || *r.ExitCode != 5 {
			return fmt.Errorf("attach stdout %q result %+v", stdout.String(), r)
		}
		return nil
	})
	res.step(prefix+": read of a missing file fails with not_found", func() error {
		_, err := sb.ReadFileBytes(ctx, "/no/such/file")
		if code := ownsand.CodeOf(err); code != 5 { // connect.CodeNotFound
			return fmt.Errorf("error %v (code %v)", err, code)
		}
		return nil
	})
	res.step(prefix+": write into a missing directory fails with failed_precondition", func() error {
		err := sb.WriteFileBytes(ctx, "/no/such/dir/file", []byte("x"), 0o644)
		if code := ownsand.CodeOf(err); code != 9 { // connect.CodeFailedPrecondition
			return fmt.Errorf("error %v (code %v)", err, code)
		}
		return nil
	})
	res.step(prefix+": API key revocation ends a running exec with an authorization error", func() error {
		key, secret, err := admin.CreateAPIKey(ctx, "e2e-revoke")
		if err != nil {
			return err
		}
		kc, err := ownsand.New(ownsand.Options{Origin: c.Origin(), OrganizationID: c.OrganizationID(), APIKey: secret, Transport: mode})
		if err != nil {
			return err
		}
		ksb, err := kc.Sandbox(sb.ID())
		if err != nil {
			return err
		}
		defer ksb.Close()
		ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		st, err := ksb.Exec(ectx, ownsand.ExecOptions{Command: []string{"/bin/sh", "-c", "echo started; sleep 20; echo late"}})
		if err != nil {
			return err
		}
		ev, err := st.Recv()
		for err == nil && ev.Kind != ownsand.ExecEventStdout {
			ev, err = st.Recv()
		}
		if err != nil {
			return fmt.Errorf("before revocation: %w", err)
		}
		if err := admin.RevokeAPIKey(ctx, key.GetApiKeyId()); err != nil {
			return err
		}
		start := time.Now()
		for {
			_, err = st.Recv()
			if err != nil {
				break
			}
		}
		if errors.Is(err, io.EOF) {
			r, _ := st.Result()
			return fmt.Errorf("the exec ended normally after the revocation: %+v", r)
		}
		switch code := ownsand.CodeOf(err); code {
		case 16, 7: // unauthenticated, permission_denied
		default:
			return fmt.Errorf("error %v (code %v)", err, code)
		}
		if d := time.Since(start); d > 15*time.Second {
			return fmt.Errorf("the revocation took %v to end the stream", d)
		}
		return nil
	})
}
