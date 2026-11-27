// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubKernelDocker replaces docker for build-kernel.sh. It records its
// arguments, one per line, in $STUB_LOG_DIR/docker.args and writes the kernel
// and config using the production copy commands against the mounted stage.
const stubKernelDocker = `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"$STUB_LOG_DIR/docker.args"
out_dir=""
out_name=""
prev=""
for arg in "$@"; do
  case "$arg" in
    OUT_NAME=*) out_name="${arg#OUT_NAME=}" ;;
  esac
  if [ "$prev" = "-v" ]; then
    case "$arg" in
      *:/out) out_dir="${arg%:/out}" ;;
    esac
  fi
  prev="$arg"
done
built=vmlinux
OUT_NAME="$out_name"
cd "$STUB_LOG_DIR"
printf 'kernel' >"$built"
printf 'config' >.config
if [ "${STUB_KERNEL_LATE_DIRECTORY:-}" = yes ]; then
  mkdir "$out_dir/$out_name"
  printf 'keep' >"$out_dir/$out_name/sentinel"
  # Record the directory before the host removes the stage on failure.
  trap 'command cp -R -- "$out_dir/$out_name" "$STUB_LOG_DIR/staged-directory"' EXIT
fi
if [ -n "${STUB_KERNEL_FINAL_DIRECTORY:-}" ]; then
  mkdir "$STUB_KERNEL_FINAL_DIRECTORY"
  printf 'keep' >"$STUB_KERNEL_FINAL_DIRECTORY/sentinel"
fi
cp() {
  # Model the container's GNU cp -T on both Linux and macOS hosts.
  local no_target_directory=false
  if [ "$1" = -T ]; then
    no_target_directory=true
    shift
  fi
  if [ "$1" = -- ]; then
    shift
  fi
  if "$no_target_directory" && [ -d "$out_dir/${2#/out/}" ]; then
    echo "output path is a directory: $2" >&2
    return 1
  fi
  if [ "${STUB_KERNEL_CONFIG_COPY_FAIL:-}" = yes ] && [ "$1" = .config ]; then
    exit 37
  fi
  command cp -- "$1" "$out_dir/${2#/out/}"
}
copies="$(printf '%s\n' "${!#}" | sed -n '/^    cp /p')"
[ -n "$copies" ]
eval "$copies"
exit "${STUB_KERNEL_BUILD_STATUS:-0}"
`

// stubShasum checks the input and prints the known fixture checksum.
const stubShasum = `#!/usr/bin/env bash
IFS= read -r input || :
[ "$#" -eq 2 ] && [ "$1" = -a ] && [ "$2" = 256 ] && [ "$input" = kernel ] || exit 4
printf '%s  -\n' '` + fixtureKernelSHA256 + `'
`

const stubFailedShasum = "#!/usr/bin/env bash\nexit 3\n"

const fixtureKernelSHA256 = "6923dd1bc0460082c5d55a831908c24a282860b7f1cd6c2b79cf1bc8857c639c"

const stubSha256sum = `#!/usr/bin/env bash
IFS= read -r input || :
[ "$#" -eq 0 ] && [ "$input" = kernel ] || exit 4
printf '%s  -\n' '` + fixtureKernelSHA256 + `'
`

type kernelBuild struct {
	t      *testing.T
	script string
	stub   string // directory with the stub docker and shasum
	logs   string
	work   string // working directory of the script, where ./vmlinux goes
	home   string
}

func newKernelBuild(t *testing.T) *kernelBuild {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the kernel build script runs on Linux and macOS hosts, not on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not available: %v", err)
	}
	script, err := filepath.Abs("build-kernel.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b := &kernelBuild{
		t:      t,
		script: script,
		stub:   filepath.Join(dir, "bin"),
		logs:   filepath.Join(dir, "logs"),
		work:   filepath.Join(dir, "work"),
		home:   filepath.Join(dir, "home"),
	}
	for _, d := range []string{b.stub, b.logs, b.work, b.home} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"docker": stubKernelDocker, "shasum": stubShasum} {
		if err := os.WriteFile(filepath.Join(b.stub, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"bash", "basename", "cp", "dirname", "mkdir", "mktemp", "mv", "rm", "rmdir", "sed"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(b.stub, name)); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// run runs build-kernel.sh from the working directory with the given
// OWNSAND_KERNEL_CACHE (unset when cache is nil) and returns its combined
// output and exit error.
func (b *kernelBuild) run(cache *string, extra ...string) (string, error) {
	b.t.Helper()
	cmd := exec.Command("bash", b.script, "amd64", "./vmlinux")
	cmd.Dir = b.work
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "OWNSAND_KERNEL_CACHE=") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env,
		"HOME="+b.home,
		"PATH="+b.stub,
		"STUB_LOG_DIR="+b.logs,
	)
	if cache != nil {
		cmd.Env = append(cmd.Env, "OWNSAND_KERNEL_CACHE="+*cache)
	}
	cmd.Env = append(cmd.Env, extra...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// dockerArgs returns the arguments of the recorded docker call, or nil when
// docker did not run.
func (b *kernelBuild) dockerArgs() []string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.logs, "docker.args"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// cacheMounts returns every -v argument that mounts at /cache.
func cacheMounts(args []string) []string {
	var mounts []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-v" && strings.HasSuffix(args[i+1], ":/cache") {
			mounts = append(mounts, args[i+1])
		}
	}
	return mounts
}

// wantCacheMount fails the test unless docker ran with exactly one cache
// mount of the host directory dir, which must exist.
func (b *kernelBuild) wantCacheMount(dir string) {
	b.t.Helper()
	args := b.dockerArgs()
	if args == nil {
		b.t.Fatal("docker did not run")
	}
	mounts := cacheMounts(args)
	if len(mounts) != 1 || mounts[0] != dir+":/cache" {
		b.t.Fatalf("cache mounts = %q, want [%q]", mounts, dir+":/cache")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		b.t.Fatalf("the cache directory %q does not exist as a directory: %v", dir, err)
	}
}

// realPath resolves symbolic links of a directory the way the script's
// `cd && pwd -P` does, for example /var to /private/var on macOS.
func realPath(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// A bare name is a Docker named volume when it reaches docker unchanged, so
// the script mounts the directory that it created under the working
// directory by its absolute path, where existing archives are visible.
func TestBuildKernelBareCacheNameMountsDirectoryUnderWorkingDirectory(t *testing.T) {
	b := newKernelBuild(t)
	seeded := filepath.Join(b.work, "kernel-cache")
	if err := os.Mkdir(seeded, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seeded, "linux-6.18.64.tar.xz"), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "kernel-cache"
	if out, err := b.run(&name); err != nil {
		t.Fatalf("build-kernel.sh failed: %v\n%s", err, out)
	}
	b.wantCacheMount(filepath.Join(realPath(t, b.work), "kernel-cache"))
	if _, err := os.Stat(filepath.Join(seeded, "linux-6.18.64.tar.xz")); err != nil {
		t.Fatalf("the seeded archive is not in the mounted directory: %v", err)
	}
}

// A bare name whose directory does not exist yet is created under the
// working directory.
func TestBuildKernelBareCacheNameCreatesDirectory(t *testing.T) {
	b := newKernelBuild(t)
	name := "fresh-cache"
	if out, err := b.run(&name); err != nil {
		t.Fatalf("build-kernel.sh failed: %v\n%s", err, out)
	}
	b.wantCacheMount(filepath.Join(realPath(t, b.work), "fresh-cache"))
}

// An absolute override with a space stays one quoted argument.
func TestBuildKernelAbsoluteCacheWithSpaceIsOneArgument(t *testing.T) {
	b := newKernelBuild(t)
	cache := filepath.Join(t.TempDir(), "kernel cache dir")
	if out, err := b.run(&cache); err != nil {
		t.Fatalf("build-kernel.sh failed: %v\n%s", err, out)
	}
	b.wantCacheMount(filepath.Join(realPath(t, filepath.Dir(cache)), "kernel cache dir"))
}

// Without an override the cache is under $HOME/.cache/ownsand-kernel.
func TestBuildKernelDefaultCacheIsUnderHome(t *testing.T) {
	b := newKernelBuild(t)
	if out, err := b.run(nil); err != nil {
		t.Fatalf("build-kernel.sh failed: %v\n%s", err, out)
	}
	b.wantCacheMount(filepath.Join(realPath(t, b.home), ".cache", "ownsand-kernel"))
}

// A cache path that cannot become a directory stops the script with a
// non-zero status before docker runs.
func TestBuildKernelCachePathThatIsAFileFailsBeforeDocker(t *testing.T) {
	b := newKernelBuild(t)
	file := filepath.Join(b.work, "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{file, "not-a-directory"} {
		if out, err := b.run(&cache); err == nil {
			t.Fatalf("build-kernel.sh with cache %q succeeded, want failure\n%s", cache, out)
		}
		if args := b.dockerArgs(); args != nil {
			t.Fatalf("docker ran with cache %q: %q", cache, args)
		}
	}
}

func TestBuildKernelRejectsDirectoryAtOutput(t *testing.T) {
	for _, name := range []string{"vmlinux", "vmlinux.config"} {
		for _, when := range []string{"existing", "during build"} {
			t.Run(name+"/"+when, func(t *testing.T) {
				b := newKernelBuild(t)
				path := filepath.Join(b.work, name)
				other := "vmlinux"
				if name == other {
					other = "vmlinux.config"
				}
				b.writeOutput(other, "old-"+other)
				var env []string
				if when == "existing" {
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
					b.writeOutput(filepath.Join(name, "sentinel"), "keep")
				} else {
					env = append(env, "STUB_KERNEL_FINAL_DIRECTORY="+path)
				}
				out, err := b.run(nil, env...)
				requireBuildFailure(t, out, err, "kernel output path is a directory:")
				if !strings.Contains(out, name+"\n") {
					t.Fatalf("failure did not name %s:\n%s", name, out)
				}
				if ran := b.dockerArgs() != nil; ran != (when == "during build") {
					t.Fatalf("docker ran = %t with %s directory", ran, when)
				}
				requireSentinelDirectory(t, path)
				b.requireOutput(other, "old-"+other)
				b.requireNoStage()
			})
		}
	}
}

func TestBuildKernelRejectsDirectoryAtStagedKernel(t *testing.T) {
	t.Run("during build", func(t *testing.T) {
		b := newKernelBuild(t)
		out, err := b.run(nil, "STUB_KERNEL_LATE_DIRECTORY=yes")
		requireSentinelDirectory(t, filepath.Join(b.logs, "staged-directory"))
		requireBuildFailure(t, out, err, "output path is a directory: /out/vmlinux")
		b.requireOutputPair("", "")
		b.requireNoStage()
	})
}

func requireSentinelDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("directory %s should contain only the sentinel, got %v, %v", path, entries, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "sentinel"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("sentinel = %q, %v", data, err)
	}
}

func requireBuildFailure(t *testing.T, out string, err error, diagnostic string) {
	t.Helper()
	if err == nil || !strings.Contains(out, diagnostic) || strings.Contains(out, "wrote ") {
		t.Fatalf("want a failure with %q and no success message, got %v\n%s", diagnostic, err, out)
	}
}

func (b *kernelBuild) writeOutput(name, contents string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.work, name), []byte(contents), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *kernelBuild) requireOutput(name, want string) {
	b.t.Helper()
	got, err := os.ReadFile(filepath.Join(b.work, name))
	if want == "" && os.IsNotExist(err) {
		return
	}
	if want == "" || err != nil || string(got) != want {
		b.t.Fatalf("%s = %q, %v, want %q (empty means absent)", name, got, err, want)
	}
}

func (b *kernelBuild) requireOutputPair(kernel, config string) {
	b.t.Helper()
	b.requireOutput("vmlinux", kernel)
	b.requireOutput("vmlinux.config", config)
}

func (b *kernelBuild) requireNoStage() {
	b.t.Helper()
	matches, err := filepath.Glob(filepath.Join(b.work, ".ownsand-kernel-*"))
	if err != nil || len(matches) != 0 {
		b.t.Fatalf("want no staging or lock items, got %v, %v", matches, err)
	}
}

func TestBuildKernelContainerFailurePreservesOutputs(t *testing.T) {
	for _, failure := range []string{"STUB_KERNEL_CONFIG_COPY_FAIL=yes", "STUB_KERNEL_BUILD_STATUS=23"} {
		for _, prior := range []string{"first build", "rebuild"} {
			t.Run(failure+"/"+prior, func(t *testing.T) {
				b := newKernelBuild(t)
				kernel, config := "", ""
				if prior == "rebuild" {
					kernel, config = "old-kernel", "old-config"
					b.writeOutput("vmlinux", kernel)
					b.writeOutput("vmlinux.config", config)
				}
				out, err := b.run(nil, failure)
				requireBuildFailure(t, out, err, "kernel build failed for ./vmlinux")
				b.requireOutputPair(kernel, config)
				b.requireNoStage()
			})
		}
	}
}

const stubKernelMove = `#!/usr/bin/env bash
set -euo pipefail
case "$3" in
  */out/vmlinux.config)
    case "${STUB_KERNEL_MOVE_FAILURE:-}" in
      sidecar) exit 42 ;;
      late-directory)
        mkdir "${4}vmlinux.config"
        printf 'keep' >"${4}vmlinux.config/sentinel"
        ;;
    esac
    ;;
  */previous/vmlinux)
    if [ "${STUB_KERNEL_RESTORE_FAILURE:-}" = yes ]; then
      exit 43
    fi
    ;;
esac
exec "$STUB_REAL_MV" "$@"
`

func (b *kernelBuild) stubMove() string {
	b.t.Helper()
	path := filepath.Join(b.stub, "mv")
	realMV, err := os.Readlink(path)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stubKernelMove), 0o755); err != nil {
		b.t.Fatal(err)
	}
	return "STUB_REAL_MV=" + realMV
}

func TestBuildKernelPublicationRollsBack(t *testing.T) {
	for _, prior := range []string{"first build", "rebuild"} {
		t.Run(prior, func(t *testing.T) {
			b := newKernelBuild(t)
			realMV := b.stubMove()
			kernel, config := "", ""
			if prior == "rebuild" {
				kernel, config = "old-kernel", "old-config"
				b.writeOutput("vmlinux", kernel)
				b.writeOutput("vmlinux.config", config)
			}
			out, err := b.run(nil, realMV, "STUB_KERNEL_MOVE_FAILURE=sidecar")
			requireBuildFailure(t, out, err, "could not publish kernel output: "+filepath.Join(realPath(t, b.work), "vmlinux.config"))
			b.requireOutputPair(kernel, config)
			b.requireNoStage()

			out, err = b.run(nil, realMV)
			if err != nil || !strings.Contains(out, "wrote ./vmlinux ("+fixtureKernelSHA256+")") {
				t.Fatalf("retry failed: %v\n%s", err, out)
			}
			b.requireOutputPair("kernel", "config")
			b.requireNoStage()
		})
	}
}

func TestBuildKernelPublicationPreservesLateDirectory(t *testing.T) {
	b := newKernelBuild(t)
	b.writeOutput("vmlinux", "old-kernel")
	path := filepath.Join(realPath(t, b.work), "vmlinux.config")
	out, err := b.run(nil, b.stubMove(), "STUB_KERNEL_MOVE_FAILURE=late-directory")
	requireBuildFailure(t, out, err, "could not publish kernel output: "+path)
	requireSentinelDirectory(t, path)
	b.requireOutput("vmlinux", "old-kernel")
	b.requireNoStage()
}

func TestBuildKernelPublicationDoesNotTakeAnOccupiedLock(t *testing.T) {
	b := newKernelBuild(t)
	b.writeOutput("vmlinux", "old-kernel")
	b.writeOutput("vmlinux.config", "old-config")
	const lock = ".ownsand-kernel-publish.lock"
	path := filepath.Join(b.work, lock)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	b.writeOutput(filepath.Join(lock, "sentinel"), "keep")
	out, err := b.run(nil)
	requireBuildFailure(t, out, err, "could not acquire kernel publication lock:")
	b.requireOutputPair("old-kernel", "old-config")
	requireSentinelDirectory(t, path)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	b.requireNoStage()
}

func TestBuildKernelRetainsRecoveryFiles(t *testing.T) {
	b := newKernelBuild(t)
	b.writeOutput("vmlinux", "old-kernel")
	b.writeOutput("vmlinux.config", "old-config")
	out, err := b.run(nil, b.stubMove(), "STUB_KERNEL_MOVE_FAILURE=sidecar", "STUB_KERNEL_RESTORE_FAILURE=yes")
	work := realPath(t, b.work)
	requireBuildFailure(t, out, err, "could not restore "+filepath.Join(work, "vmlinux")+"\n")
	stages, err := filepath.Glob(filepath.Join(work, ".ownsand-kernel-stage.*"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("want one retained stage, got %v, %v", stages, err)
	}
	if !strings.Contains(out, "kernel rollback incomplete; recovery files are in "+stages[0]) {
		t.Fatalf("failure did not identify the retained stage:\n%s", out)
	}
	saved, err := os.ReadFile(filepath.Join(stages[0], "previous", "vmlinux"))
	if err != nil || string(saved) != "old-kernel" {
		t.Fatalf("saved kernel = %q, %v", saved, err)
	}
	b.requireOutputPair("kernel", "old-config")
}

func TestBuildKernelChecksumTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shasum   string
		fallback bool
		want     string
	}{
		{"preferred tool fails", stubFailedShasum, true, "could not compute SHA-256 for ./vmlinux"},
		{"malformed checksum", "#!/usr/bin/env bash\nprintf 'bad checksum\\n'\n", true, "could not compute SHA-256 for ./vmlinux"},
		{"both tools absent", "", false, "install shasum or sha256sum"},
		{"fallback succeeds", "", true, "wrote ./vmlinux (" + fixtureKernelSHA256 + ")"},
	} {
		for _, prior := range []string{"first build", "rebuild"} {
			t.Run(tc.name+"/"+prior, func(t *testing.T) {
				b := newKernelBuild(t)
				kernel, config := "", ""
				if prior == "rebuild" {
					kernel, config = "old-kernel", "old-config"
					b.writeOutput("vmlinux", kernel)
					b.writeOutput("vmlinux.config", config)
				}
				path := filepath.Join(b.stub, "shasum")
				if tc.shasum == "" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(tc.shasum), 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.fallback {
					if err := os.WriteFile(filepath.Join(b.stub, "sha256sum"), []byte(stubSha256sum), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				out, err := b.run(nil)
				wantSuccess := tc.shasum == "" && tc.fallback
				if (err == nil) != wantSuccess || strings.Contains(out, "wrote ") != wantSuccess || !strings.Contains(out, tc.want) {
					t.Fatalf("want %q (success = %t), got %v\n%s", tc.want, wantSuccess, err, out)
				}
				if wantSuccess {
					kernel, config = "kernel", "config"
				}
				b.requireOutputPair(kernel, config)
				b.requireNoStage()
			})
		}
	}
}
