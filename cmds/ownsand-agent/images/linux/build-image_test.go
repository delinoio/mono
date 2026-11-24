// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubGo replaces go. For `go build ... -o <path>` it records $GOARCH and
// writes the freshly built "binary" to <path>, or it fails with
// $STUB_GO_STATUS (default 0) after creating a partial file at <path>.
const stubGo = `#!/usr/bin/env bash
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then out="$arg"; fi
  prev="$arg"
done
printf '%s\n' "$GOARCH" >"$STUB_LOG_DIR/go.goarch"
printf 'built' >"$out"
exit "${STUB_GO_STATUS:-0}"
`

// stubDocker replaces docker. It records its arguments, one per line, and
// reads what the container would see: the guest helper behind the
// read-only /stage mount. When $STUB_DOCKER_STATUS is 0, it also writes the
// root file system into the /out mount, like build-rootfs.sh does; otherwise
// it fails with that status.
const stubDocker = `#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_LOG_DIR/docker.args"
stage=""
out=""
arch=""
for arg in "$@"; do
  case "$arg" in
    *:/stage:ro) stage="${arg%:/stage:ro}" ;;
    *:/out) out="${arg%:/out}" ;;
    linux/*) arch="${arg#linux/}" ;;
  esac
done
cp "$stage/ownsand-guest" "$STUB_LOG_DIR/staged.bin"
printf '%s\n' "$stage" >"$STUB_LOG_DIR/stage.path"
if [ "${STUB_DOCKER_STATUS:-0}" != 0 ]; then
  printf 'partial' >"$out/rootfs.ext4"
  exit "$STUB_DOCKER_STATUS"
fi
printf 'rootfs-%s' "$arch" >"$out/rootfs.ext4"
`

// stubKernel replaces build-kernel.sh. It writes the kernel and its config,
// or fails with $STUB_KERNEL_STATUS before writing the config.
const stubKernel = `#!/usr/bin/env bash
printf '%s\n' "$1" >"$STUB_LOG_DIR/kernel.arch"
printf 'kernel-%s' "$1" >"$2"
if [ "${STUB_KERNEL_STATUS:-0}" != 0 ]; then
  exit "$STUB_KERNEL_STATUS"
fi
printf 'config-%s' "$1" >"$2.config"
`

// imageBuild is one prepared run of build-image.sh in a fake repository that
// has stub go, docker and build-kernel.sh.
type imageBuild struct {
	t      *testing.T
	script string
	dir    string
	out    string
	logs   string
	tmp    string
	env    []string
}

func newImageBuild(t *testing.T) *imageBuild {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the image build runs on Linux and macOS hosts")
	}
	for _, tool := range []string{"bash", "mktemp", "cp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	real, err := os.ReadFile("build-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	scripts := filepath.Join(dir, "repo", "cmds", "ownsand-agent", "images", "linux")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scripts, "build-image.sh")
	if err := os.WriteFile(script, real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "build-kernel.sh"), []byte(stubKernel), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	logs := filepath.Join(dir, "logs")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{bin, logs, tmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"go": stubGo, "docker": stubDocker} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &imageBuild{
		t:      t,
		script: script,
		dir:    dir,
		out:    filepath.Join(dir, "out"),
		logs:   logs,
		tmp:    tmp,
		env: []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"STUB_LOG_DIR=" + logs,
			"TMPDIR=" + tmp,
		},
	}
}

func (b *imageBuild) run(arch string, extra ...string) (string, error) {
	cmd := exec.Command("bash", b.script, arch, b.out)
	cmd.Env = append(append(os.Environ(), b.env...), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seedGuest puts a file at <output-dir>/ownsand-guest that the build must not
// touch.
func (b *imageBuild) seedGuest(data []byte) {
	b.t.Helper()
	if err := os.MkdirAll(b.out, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.out, "ownsand-guest"), data, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *imageBuild) readLog(name string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.logs, name))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

// requireGuest checks that <output-dir>/ownsand-guest holds want, or does not
// exist when want is nil.
func (b *imageBuild) requireGuest(want []byte) {
	b.t.Helper()
	got, err := os.ReadFile(filepath.Join(b.out, "ownsand-guest"))
	if want == nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.t.Fatalf("the build left an ownsand-guest file behind (read error is nil: %t)", err == nil)
		}
		return
	}
	if err != nil {
		b.t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		b.t.Fatalf("ownsand-guest changed: got %q, want %q", got, want)
	}
}

func imageOutputs(arch string) map[string]string {
	return map[string]string{
		"vmlinux":        "kernel-" + arch,
		"vmlinux.config": "config-" + arch,
		"rootfs.ext4":    "rootfs-" + arch,
	}
}

func (b *imageBuild) seedImage(arch string) {
	b.t.Helper()
	if err := os.MkdirAll(b.out, 0o755); err != nil {
		b.t.Fatal(err)
	}
	for name, data := range imageOutputs(arch) {
		if err := os.WriteFile(filepath.Join(b.out, name), []byte(data), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
}

// An empty arch means that none of the final image paths should exist.
func (b *imageBuild) requireImage(arch string) {
	b.t.Helper()
	for name, want := range imageOutputs(arch) {
		path := filepath.Join(b.out, name)
		if arch == "" {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				b.t.Fatalf("%s: want an absent path, got stat error %v", name, err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			b.t.Fatalf("%s: got %q, want %q (read error: %v)", name, got, want, err)
		}
	}
}

// requireNoStage checks that no staging directory is left in the output
// directory and that no scratch is left in TMPDIR.
func (b *imageBuild) requireNoStage() {
	b.t.Helper()
	for _, dir := range []string{b.out, b.tmp} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			b.t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), "stage") || e.Name() == ".ownsand-image-publish.lock" {
				b.t.Fatalf("the build left %q in %s", e.Name(), filepath.Base(dir))
			}
		}
	}
}

func TestSuccessfulBuildPreservesAnExistingGuestFile(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			b := newImageBuild(t)
			sentinel := []byte("pre-existing\x00bytes\n")
			b.seedGuest(sentinel)
			if out, err := b.run(arch); err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			b.requireGuest(sentinel)
			b.requireNoStage()
			b.requireImage(arch)
			if got := strings.TrimSpace(b.readLog("go.goarch")); got != arch {
				t.Fatalf("go built for %q, want %q", got, arch)
			}
			if got := strings.TrimSpace(b.readLog("kernel.arch")); got != arch {
				t.Fatalf("kernel built for %q, want %q", got, arch)
			}
		})
	}
}

func TestSuccessfulBuildLeavesNoGuestFileWhenThereWasNone(t *testing.T) {
	b := newImageBuild(t)
	if out, err := b.run("amd64"); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	b.requireGuest(nil)
	b.requireNoStage()
}

func TestRootfsContainerReadsTheGuestFromAReadOnlyStage(t *testing.T) {
	b := newImageBuild(t)
	b.seedGuest([]byte("pre-existing"))
	if out, err := b.run("arm64"); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	args := strings.Split(strings.TrimSpace(b.readLog("docker.args")), "\n")
	var stage, outMount, guestEnv bool
	for i, arg := range args {
		switch {
		case arg == "-v" && i+1 < len(args) && strings.HasSuffix(args[i+1], ":/stage:ro"):
			stage = true
		case arg == "-v" && i+1 < len(args) && strings.HasSuffix(args[i+1], ":/out"):
			outMount = true
		case strings.Contains(arg, "GUEST_BINARY=/stage/ownsand-guest"):
			guestEnv = true
		}
	}
	if !stage || !outMount || !guestEnv {
		t.Fatalf("docker run arguments: read-only stage mount %t, output mount %t, guest from /stage %t\n%s",
			stage, outMount, guestEnv, strings.Join(args, "\n"))
	}
	if got := b.readLog("staged.bin"); got != "built" {
		t.Fatalf("the container saw the guest helper %q, want the freshly built one", got)
	}
	stagePath := strings.TrimSpace(b.readLog("stage.path"))
	if filepath.Dir(stagePath) == stagePath || stagePath == b.out || filepath.Base(stagePath) == "ownsand-guest" {
		t.Fatalf("the stage directory %q is not a dedicated directory", stagePath)
	}
}

func TestBuildKeepsTheStageOfAnotherRun(t *testing.T) {
	b := newImageBuild(t)
	// A build that runs at once in the same output directory owns a stage
	// directory of its own; this build must neither reuse nor remove it.
	other := filepath.Join(b.out, ".ownsand-image-stage.other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "ownsand-guest"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := b.run("amd64"); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(other, "ownsand-guest"))
	if err != nil || string(got) != "other" {
		t.Fatalf("the stage of the other run: got %q (read error is nil: %t)", got, err == nil)
	}
	if got := b.readLog("staged.bin"); got != "built" {
		t.Fatalf("the container saw %q, want the freshly built guest helper", got)
	}
	if stagePath := strings.TrimSpace(b.readLog("stage.path")); filepath.Dir(stagePath) == other {
		t.Fatal("the build reused the stage directory of another run")
	}
}

func TestFailedStepsPreserveThePreviousOutputs(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		status int
	}{
		{"go build", "STUB_GO_STATUS=7", 7},
		{"root file system container", "STUB_DOCKER_STATUS=23", 23},
		{"kernel build", "STUB_KERNEL_STATUS=9", 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newImageBuild(t)
			sentinel := []byte("pre-existing\x00bytes\n")
			b.seedGuest(sentinel)
			b.seedImage("amd64")
			out, err := b.run("arm64", c.env)
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the build did not fail with an exit status: %v\n%s", err, out)
			}
			if exit.ExitCode() != c.status {
				t.Fatalf("exit status %d, want %d\n%s", exit.ExitCode(), c.status, out)
			}
			b.requireGuest(sentinel)
			b.requireImage("amd64")
			b.requireNoStage()
		})
	}
}

func TestFailedFirstBuildLeavesNoOutputs(t *testing.T) {
	for _, env := range []string{"STUB_GO_STATUS=7", "STUB_DOCKER_STATUS=23", "STUB_KERNEL_STATUS=9"} {
		t.Run(env, func(t *testing.T) {
			b := newImageBuild(t)
			if _, err := b.run("arm64", env); err == nil {
				t.Fatal("the build succeeded, want a failure")
			}
			b.requireGuest(nil)
			b.requireImage("")
			b.requireNoStage()
		})
	}
}

func TestPublicationFailureRollsBackAndAllowsRetry(t *testing.T) {
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		arch string
	}{
		{"first build", ""},
		{"rebuild", "amd64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newImageBuild(t)
			sentinel := []byte("unrelated")
			b.seedGuest(sentinel)
			if c.arch != "" {
				b.seedImage(c.arch)
			}
			stubPath := filepath.Join(b.dir, "bin", "mv")
			const fault = `#!/usr/bin/env bash
if [[ "$3" == */out/vmlinux.config ]]; then
  # Confirm the first rename completed before failing the second.
  if [ "$(cat "${4%/}/vmlinux")" != kernel-arm64 ]; then
    exit 86
  fi
  exit 87
fi
exec "$STUB_REAL_MV" "$@"
`
			if err := os.WriteFile(stubPath, []byte(fault), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := b.run("arm64", "STUB_REAL_MV="+realMV)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 87 {
				t.Fatalf("want the second rename to fail with exit 87, got %v\n%s", err, out)
			}
			b.requireImage(c.arch)
			b.requireGuest(sentinel)
			b.requireNoStage()

			if err := os.Remove(stubPath); err != nil {
				t.Fatal(err)
			}
			if out, err := b.run("arm64"); err != nil {
				t.Fatalf("retry failed: %v\n%s", err, out)
			}
			b.requireImage("arm64")
			b.requireGuest(sentinel)
			b.requireNoStage()
		})
	}
}

func TestPublicationRejectsADirectoryAtAnOutputPath(t *testing.T) {
	b := newImageBuild(t)
	config := filepath.Join(b.out, "vmlinux.config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := b.run("arm64"); err == nil || !strings.Contains(out, "output path is a directory: "+config) {
		t.Fatalf("want an output directory error, got %v\n%s", err, out)
	}
	entries, err := os.ReadDir(b.out)
	if err != nil || len(entries) != 1 || entries[0].Name() != "vmlinux.config" || !entries[0].IsDir() {
		t.Fatalf("want only the existing directory, got %v (read error: %v)", entries, err)
	}
	entries, err = os.ReadDir(config)
	if err != nil || len(entries) != 0 {
		t.Fatalf("want the existing directory to stay empty, got %v (read error: %v)", entries, err)
	}
}

func TestBuildDoesNotPublishWhileAnotherRunHoldsTheLock(t *testing.T) {
	b := newImageBuild(t)
	b.seedImage("amd64")
	lock := filepath.Join(b.out, ".ownsand-image-publish.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := b.run("arm64"); err == nil || !strings.Contains(out, "could not acquire image publication lock: "+lock) {
		t.Fatalf("want a publication lock error, got %v\n%s", err, out)
	}
	b.requireImage("amd64")
	if err := os.Remove(lock); err != nil {
		t.Fatalf("the build did not preserve the other run's lock: %v", err)
	}
	b.requireNoStage()
}
