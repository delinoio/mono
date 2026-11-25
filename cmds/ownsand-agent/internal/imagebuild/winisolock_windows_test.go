// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const isoProcessPath = "OWNSAND_TEST_ISO_PROCESS_PATH"

func TestMountISOWindowsProcessHelper(t *testing.T) {
	path := os.Getenv(isoProcessPath)
	if path == "" {
		t.Skip("helper process for the Windows ISO lock tests")
	}
	wait, err := time.ParseDuration(os.Getenv("OWNSAND_TEST_ISO_WAIT"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	lock := lockWindowsISO
	run := powershell
	dir := os.Getenv("OWNSAND_TEST_ISO_LOCK_DIR")
	if dir != "" {
		lock = fixtureISOLocker(dir)
		run = (&fakeISOHost{}).run
	}
	fmt.Println("waiting")
	m, err := mountISOWith(ctx, path, func(ctx context.Context, script string, env ...string) ([]byte, error) {
		if script == isoQueryScript {
			fmt.Println("query")
		}
		return run(ctx, script, env...)
	}, lock)
	if errors.Is(err, context.DeadlineExceeded) && wait == isoCancellationWait {
		fmt.Println("canceled")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer m.finish(lockTestLogger())
	if dir == "" {
		if !m.ours {
			t.Fatal("the first process left the ISO attached")
		}
		readISOMedia(t, m)
	}
	fmt.Println("reading")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := m.dismount(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func fixtureISOLocker(dir string) isoLocker {
	return func(ctx context.Context, path string) (func(), error) {
		return lockISOFile(ctx, dir, path)
	}
}

type isoProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines <-chan string
}

const isoCancellationWait = 500 * time.Millisecond

func startISOProcess(t *testing.T, dir, path string, wait time.Duration) *isoProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMountISOWindowsProcessHelper$")
	cmd.Env = append(os.Environ(), isoProcessPath+"="+path, "OWNSAND_TEST_ISO_LOCK_DIR="+dir, "OWNSAND_TEST_ISO_WAIT="+wait.String())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for range lines {
		}
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	p := &isoProcess{cmd: cmd, stdin: stdin, lines: lines}
	p.expect(t, "waiting")
	return p
}

func (p *isoProcess) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got, ok := <-p.lines:
		if !ok || got != want {
			t.Fatalf("helper output = %q, want %q", got, want)
		}
		t.Log(got)
	case <-time.After(2 * time.Minute):
		t.Fatalf("helper did not report %q", want)
	}
}

func (p *isoProcess) expectBlocked(t *testing.T) {
	t.Helper()
	select {
	case got := <-p.lines:
		t.Fatalf("helper proceeded while the ISO was locked: %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func (p *isoProcess) finish(t *testing.T) {
	t.Helper()
	if err := p.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for line := range p.lines {
		t.Log(line)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestMountISOWindowsSerializesProcesses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Windows Media.iso")
	lock := fixtureISOLocker(dir)
	logger := lockTestLogger()
	ctx := testContext(t)
	host := &fakeISOHost{}
	var waiter *isoProcess
	run := func(ctx context.Context, script string, env ...string) ([]byte, error) {
		if script == isoDismountScript && host.count("dismount") == 1 {
			// The waiter must still be blocked inside the final detach attempt.
			waiter.expectBlocked(t)
		}
		return host.run(ctx, script, env...)
	}
	first, err := mountISOWith(ctx, path, run, lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.releaseLock)

	canceled := startISOProcess(t, dir, strings.ToUpper(path), isoCancellationWait)
	canceled.expect(t, "canceled")
	canceled.finish(t)

	other := startISOProcess(t, dir, filepath.Join(dir, "other.iso"), time.Minute)
	other.expect(t, "query")
	other.expect(t, "reading")
	other.finish(t)

	waiter = startISOProcess(t, dir, strings.ToUpper(path), time.Minute)
	waiter.expectBlocked(t)
	host.dismountErr = errors.New("transient dismount failure")
	first.release(ctx, logger)
	host.dismountErr = nil
	first.finish(logger)
	if got := host.count("dismount"); got != 2 || host.attached {
		t.Fatalf("dismounts = %d, attached = %t", got, host.attached)
	}
	waiter.expect(t, "query")
	waiter.expect(t, "reading")
	waiter.finish(t)
}

func TestMountISOWindowsReleasesLockOnProcessExit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Windows Media.iso")
	holder := startISOProcess(t, dir, path, time.Minute)
	holder.expect(t, "query")
	holder.expect(t, "reading")
	waiter := startISOProcess(t, dir, path, time.Minute)
	waiter.expectBlocked(t)
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waiter.expect(t, "query")
	waiter.expect(t, "reading")
	waiter.finish(t)
}

func TestMountISOWindowsLeavesPreAttachedMediaAttached(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Windows Media.iso")
	ctx := testContext(t)
	host := &fakeISOHost{attached: true}
	m, err := mountISOWith(ctx, path, host.run, fixtureISOLocker(dir))
	if err != nil {
		t.Fatal(err)
	}
	m.finish(lockTestLogger())
	if !host.attached || host.count("mount") != 0 || host.count("dismount") != 0 {
		t.Fatalf("the build changed a pre-attached ISO: %v", host.steps)
	}
	waiter := startISOProcess(t, dir, path, time.Minute)
	waiter.expect(t, "query")
	waiter.expect(t, "reading")
	waiter.finish(t)
}

// Run this on an elevated test host with a detached local x64 installation ISO.
// Only the mount and media read run. This test does not boot a VM.
func TestMountISOWindowsRealMedia(t *testing.T) {
	path := os.Getenv("OWNSAND_IMAGEBUILD_TEST_ISO")
	if path == "" {
		t.Skip("set OWNSAND_IMAGEBUILD_TEST_ISO to a detached x64 installation ISO")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	first, err := mountISO(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ours {
		first.finish(lockTestLogger())
		t.Fatal("start the test with the ISO detached")
	}
	t.Cleanup(func() {
		first.finish(lockTestLogger())
		// The ISO started detached. Restore that state if a helper stopped
		// before it completed cleanup or the pre-attached case failed.
		ctx, cancel := context.WithTimeout(context.Background(), isoDismountTimeout)
		defer cancel()
		attached, err := first.queryAttached(ctx)
		if err != nil {
			t.Errorf("query the test ISO during cleanup: %v", err)
		} else if attached {
			if _, err := powershell(ctx, isoDismountScript, "OWNSAND_ISO="+path); err != nil {
				t.Errorf("detach the test ISO during cleanup: %v", err)
			}
		}
	})
	waiter := startISOProcess(t, "", strings.ToUpper(path), time.Minute)
	waiter.expectBlocked(t)
	readISOMedia(t, first)
	if err := first.dismount(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("first process finished reading and dismounted the ISO")
	waiter.expect(t, "query")
	waiter.expect(t, "reading")
	waiter.finish(t)
	attached, err := first.queryAttached(ctx)
	if err != nil || attached {
		t.Fatalf("final attach state = %t, err = %v", attached, err)
	}
	if _, err := powershell(ctx, isoMountScript, "OWNSAND_ISO="+path); err != nil {
		t.Fatal(err)
	}
	preAttached, err := mountISO(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer preAttached.finish(lockTestLogger())
	if preAttached.ours {
		t.Fatal("the build took ownership of a pre-attached ISO")
	}
	readISOMedia(t, preAttached)
	preAttached.finish(lockTestLogger())
	attached, err = first.queryAttached(ctx)
	if err != nil || !attached {
		t.Fatalf("pre-attached media state = %t, err = %v", attached, err)
	}
	t.Log("the pre-attached ISO stayed attached after the build")
}

func readISOMedia(t *testing.T, m *mountedISO) {
	t.Helper()
	wim, err := m.installImage()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(wim)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var prefix [8]byte
	if _, err := io.ReadFull(file, prefix[:]); err != nil {
		t.Fatal(err)
	}
}
