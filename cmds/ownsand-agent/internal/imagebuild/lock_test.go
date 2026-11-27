// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	lockHelperEnv         = "IMAGEBUILD_LOCK_HELPER_DIR"
	lockHelperReady       = "holding"
	lockHolderTimeout     = 10 * time.Second
	lockContenderDirEnv   = "IMAGEBUILD_LOCK_CONTENDER_DIR"
	lockContenderGuestEnv = "IMAGEBUILD_LOCK_CONTENDER_GUEST"
	lockContenderBinEnv   = "IMAGEBUILD_LOCK_CONTENDER_GUEST_BINARY"
	lockContenderTimeout  = 5 * time.Second
	stagingSentinel       = "sentinel"
)

func lockTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestLockHolderHelper is not a test: the multi-process tests re-execute the
// test binary with lockHelperEnv set, and this function then plays a build
// that holds the lock of the image directory until its stdin closes or the
// parent kills the process.
func TestLockHolderHelper(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("helper process of the multi-process lock tests")
	}
	err := withBuildLock(dir, lockTestLogger(), func() error {
		// Stage a sentinel the way a platform builder does.
		build := images.BuildDir(dir)
		if err := os.MkdirAll(build, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(build, stagingSentinel), []byte("staged"), 0o600); err != nil {
			return err
		}
		os.Stdout.WriteString(lockHelperReady + "\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return nil
	})
	if err != nil {
		t.Fatalf("helper build: %v", err)
	}
}

type lockHolder struct {
	process *os.Process
	stdin   io.WriteCloser
	done    chan struct{}
	waitErr error
}

// startLockHolder starts a process that holds the build lock of dir and waits
// until it holds the lock.
func startLockHolder(t *testing.T, dir string) *lockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderHelper$", "-test.v")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHolder{process: cmd.Process, stdin: stdin, done: make(chan struct{})}
	t.Cleanup(func() { h.kill(t) })
	ready := make(chan error, 1)
	go func() {
		defer func() {
			h.waitErr = cmd.Wait()
			close(h.done)
		}()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == lockHelperReady {
				ready <- nil
				io.Copy(io.Discard, stdout)
				return
			}
		}
		ready <- errors.New("the lock holder ended before it held the lock")
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lockHolderTimeout):
		t.Fatal("the lock holder did not take the lock in time")
	}
	return h
}

func (h *lockHolder) kill(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
		return
	default:
	}
	if err := h.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop the lock holder: %v", err)
	}
	_ = h.wait(t)
}

func (h *lockHolder) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.done:
		return h.waitErr
	case <-time.After(lockHolderTimeout):
		t.Fatal("the lock holder did not exit in time")
		return nil
	}
}

func imageDir(t *testing.T, imagesDir string, os ownsandv1.GuestOs) string {
	t.Helper()
	dir, err := images.Dir(imagesDir, os)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLockContenderHelper runs the contender in a separate process so the
// parent can stop it even if acquiring the lock blocks.
func TestLockContenderHelper(t *testing.T) {
	dir := os.Getenv(lockContenderDirEnv)
	if dir == "" {
		t.Skip("helper process of the multi-process lock tests")
	}
	guest, ok := ownsandv1.GuestOs_value[os.Getenv(lockContenderGuestEnv)]
	if !ok {
		t.Fatal("invalid contender guest OS")
	}
	if guestOS := ownsandv1.GuestOs(guest); guestOS != ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED {
		opts := Options{
			ImagesDir:   dir,
			GuestOS:     guestOS,
			ISO:         filepath.Join(dir, "missing.iso"),
			IPSW:        filepath.Join(dir, "missing.ipsw"),
			GuestBinary: os.Getenv(lockContenderBinEnv),
			Logger:      lockTestLogger(),
		}
		if err := Build(t.Context(), opts); !errors.Is(err, ErrBuildInProgress) {
			t.Fatalf("want ErrBuildInProgress, got %v", err)
		}
		return
	}
	ran := false
	err := withBuildLock(dir, lockTestLogger(), func() error {
		ran = true
		// A platform builder removes the staging directory first.
		return os.RemoveAll(images.BuildDir(dir))
	})
	if ran {
		t.Fatal("the build ran while another process held the lock")
	}
	if !errors.Is(err, ErrBuildInProgress) {
		t.Fatalf("want ErrBuildInProgress, got %v", err)
	}
	var inProgress *BuildInProgressError
	if !errors.As(err, &inProgress) || inProgress.Image != images.WindowsDir {
		t.Fatalf("want a *BuildInProgressError for windows, got %#v", err)
	}
	if !strings.Contains(err.Error(), "build in progress for windows image") {
		t.Fatalf("error text: %v", err)
	}
}

func runLockContender(t *testing.T, dir string, guest ownsandv1.GuestOs) {
	t.Helper()
	var guestBinary string
	if guest != ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED {
		guestBinary = fakeGuestBinary(t, guest)
	}
	// The caller keeps the holder alive until CombinedOutput stops and reaps
	// the contender. Releasing the lock first could let a blocked build run.
	ctx, cancel := context.WithTimeout(t.Context(), lockContenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockContenderHelper$")
	cmd.Env = append(os.Environ(),
		lockContenderDirEnv+"="+dir,
		lockContenderGuestEnv+"="+guest.String(),
		lockContenderBinEnv+"="+guestBinary,
	)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%s: lock contender did not return within %s\n%s", t.Name(), lockContenderTimeout, out)
		}
		t.Fatalf("lock contender failed: %v\n%s", err, out)
	}
}

func TestBuildLockRefusesASecondProcessBeforeTouchingStaging(t *testing.T) {
	dir := imageDir(t, t.TempDir(), ownsandv1.GuestOs_GUEST_OS_WINDOWS)
	h := startLockHolder(t, dir)

	runLockContender(t, dir, ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED)
	if got, err := os.ReadFile(filepath.Join(images.BuildDir(dir), stagingSentinel)); err != nil || string(got) != "staged" {
		t.Fatalf("the staging directory of the holder was modified: %q %v", got, err)
	}

	// The holder finishes normally and a later build proceeds.
	if err := h.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.wait(t); err != nil {
		t.Fatalf("the lock holder failed: %v", err)
	}
	ran := false
	if err := withBuildLock(dir, lockTestLogger(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("a build after the holder completed: ran=%v err=%v", ran, err)
	}
}

func TestBuildLockIsReleasedWhenTheHolderIsKilled(t *testing.T) {
	imagesDir := t.TempDir()
	dir := imageDir(t, imagesDir, ownsandv1.GuestOs_GUEST_OS_MACOS)
	h := startLockHolder(t, dir)
	if _, err := acquireBuildLock(dir, lockTestLogger()); !errors.Is(err, ErrBuildInProgress) {
		t.Fatalf("want ErrBuildInProgress while the holder lives, got %v", err)
	}
	h.kill(t)

	lock, err := acquireBuildLock(dir, lockTestLogger())
	if err != nil {
		t.Fatalf("the lock of a killed holder was not released: %v", err)
	}
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLockDistinctDestinationsDoNotContend(t *testing.T) {
	imagesDir := t.TempDir()
	macos := imageDir(t, imagesDir, ownsandv1.GuestOs_GUEST_OS_MACOS)
	windows := imageDir(t, imagesDir, ownsandv1.GuestOs_GUEST_OS_WINDOWS)
	h := startLockHolder(t, macos)
	defer h.kill(t)

	lock, err := acquireBuildLock(windows, lockTestLogger())
	if err != nil {
		t.Fatalf("a build of another image contended: %v", err)
	}
	defer lock.release()

	// Another images directory with the same guest OS does not contend either.
	other := imageDir(t, t.TempDir(), ownsandv1.GuestOs_GUEST_OS_MACOS)
	lock2, err := acquireBuildLock(other, lockTestLogger())
	if err != nil {
		t.Fatalf("a build in another images directory contended: %v", err)
	}
	defer lock2.release()
}

func TestBuildLockSameProcessSecondHandleContends(t *testing.T) {
	dir := imageDir(t, t.TempDir(), ownsandv1.GuestOs_GUEST_OS_WINDOWS)
	first, err := acquireBuildLock(dir, lockTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireBuildLock(dir, lockTestLogger()); !errors.Is(err, ErrBuildInProgress) {
		t.Fatalf("want ErrBuildInProgress, got %v", err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	// The lock file stays in place, and the lock is free again.
	if _, err := os.Stat(buildLockPath(dir)); err != nil {
		t.Fatalf("the lock file was removed: %v", err)
	}
	second, err := acquireBuildLock(dir, lockTestLogger())
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = second.release()
}

func TestWithBuildLockReleasesAfterAFailedBuild(t *testing.T) {
	dir := imageDir(t, t.TempDir(), ownsandv1.GuestOs_GUEST_OS_MACOS)
	boom := errors.New("boom")
	if err := withBuildLock(dir, lockTestLogger(), func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("the build error was not returned: %v", err)
	}
	if err := withBuildLock(dir, lockTestLogger(), func() error { return nil }); err != nil {
		t.Fatalf("a build after a failed build: %v", err)
	}
}

// fakeGuestBinary writes the smallest file that the guest helper check of the
// guest OS accepts: a Mach-O arm64 header for macOS and a PE amd64 header
// for Windows. Build only inspects the header before it takes the lock.
func fakeGuestBinary(t *testing.T, guest ownsandv1.GuestOs) string {
	t.Helper()
	var b []byte
	name := "ownsand-guest"
	switch guest {
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		// mach_header_64: magic, cputype arm64, cpusubtype, filetype
		// execute, ncmds, sizeofcmds, flags, reserved.
		b = make([]byte, 32)
		for i, v := range []uint32{0xfeedfacf, 0x0100000c, 0, 2, 0, 0, 0, 0} {
			b[i*4], b[i*4+1], b[i*4+2], b[i*4+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
	case ownsandv1.GuestOs_GUEST_OS_WINDOWS:
		name += ".exe"
		// An amd64 executable image that checkWindowsAMD64 accepts.
		b = minimalWindowsAMD64PE()
	default:
		t.Fatalf("no fake guest binary for %v", guest)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuildRefusesWhileAnotherProcessBuilds runs the exported Build against a
// destination whose lock another process holds, for both guest OSes.
func TestBuildRefusesWhileAnotherProcessBuilds(t *testing.T) {
	for _, guest := range []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_MACOS, ownsandv1.GuestOs_GUEST_OS_WINDOWS} {
		t.Run(guest.String(), func(t *testing.T) {
			imagesDir := t.TempDir()
			dir := imageDir(t, imagesDir, guest)
			startLockHolder(t, dir)
			runLockContender(t, imagesDir, guest)
			if _, err := os.Stat(filepath.Join(images.BuildDir(dir), stagingSentinel)); err != nil {
				t.Fatalf("Build touched the staging directory of the holder: %v", err)
			}
		})
	}
}
