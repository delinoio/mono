// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubBusybox is a busybox replacement. With --list it prints $STUB_APPLETS
// and exits with $STUB_LIST_STATUS (default 0), so a test can make the applet
// listing fail without a real busybox.
const stubBusybox = `#!/usr/bin/env bash
if [ "${1:-}" = "--list" ]; then
  [ -n "${STUB_APPLETS:-}" ] && printf '%s\n' "$STUB_APPLETS"
  exit "${STUB_LIST_STATUS:-0}"
fi
exit 0
`

// stubFile is a file replacement. It describes its last argument as
// $STUB_FILE_DESCRIPTION (default "statically linked") because the script
// rejects a busybox that file does not call static and the stub busybox is a
// shell script. Like the real file, it describes a symbolic link as a link
// unless -L is given, and it prints "cannot open" and exits 0 for a missing
// target. It prints the path unless -b is given.
const stubFile = `#!/usr/bin/env bash
follow=0
brief=0
target=""
for arg in "$@"; do
  case "$arg" in
    -L) follow=1 ;;
    -b) brief=1 ;;
    --) ;;
    *) target="$arg" ;;
  esac
done
prefix=""
if [ "$brief" = 0 ]; then
  prefix="$target: "
fi
if [ "$follow" = 0 ] && [ -L "$target" ]; then
  echo "${prefix}symbolic link to $(readlink "$target")"
elif [ ! -e "$target" ]; then
  echo "${prefix}cannot open ('No such file or directory')"
else
  echo "${prefix}ELF 64-bit LSB executable, ${STUB_FILE_DESCRIPTION:-statically linked}"
fi
`

// rootfsBuild is one prepared run of build-rootfs.sh.
type rootfsBuild struct {
	t      *testing.T
	script string
	dir    string
	out    string
	env    []string
}

func newRootfsBuild(t *testing.T) *rootfsBuild {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the root file system build runs on Linux")
	}
	for _, tool := range []string{"bash", "mktemp", "truncate"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	script, err := filepath.Abs("build-rootfs.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	busybox := filepath.Join(stubs, "busybox")
	if err := os.WriteFile(busybox, []byte(stubBusybox), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubs, "file"), []byte(stubFile), 0o755); err != nil {
		t.Fatal(err)
	}
	guest := filepath.Join(dir, "guest")
	if err := os.WriteFile(guest, []byte("guest"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	return &rootfsBuild{
		t:      t,
		script: script,
		dir:    dir,
		out:    filepath.Join(dir, "rootfs.ext4"),
		env: []string{
			"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
			"BUSYBOX=" + busybox,
			"GUEST_BINARY=" + guest,
			"TMPDIR=" + tmp,
		},
	}
}

func (b *rootfsBuild) run(extra ...string) (string, error) {
	cmd := exec.Command("bash", b.script, b.out, "8")
	cmd.Env = append(append(os.Environ(), b.env...), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireNoScratch checks that the build removed its scratch directory.
func (b *rootfsBuild) requireNoScratch() {
	b.t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.dir, "tmp"))
	if err != nil {
		b.t.Fatal(err)
	}
	if len(entries) != 0 {
		b.t.Fatalf("the build left %d scratch entries behind", len(entries))
	}
}

// requireFailure checks that err is a non-zero exit of the script.
func requireFailure(t *testing.T, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("the build succeeded, want a failure\n%s", out)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("run the build: %v", err)
	}
}

func TestFailedAppletListingFailsTheBuildAndKeepsTheOutput(t *testing.T) {
	cases := []struct {
		name    string
		env     []string
		message string
	}{
		{"list exits non-zero", []string{"STUB_APPLETS=sh\nls", "STUB_LIST_STATUS=42"}, "busybox --list failed"},
		{"list prints nothing", []string{"STUB_LIST_STATUS=0"}, "printed no applets"},
		{"list has no sh", []string{"STUB_APPLETS=ls\ncat"}, "does not provide the sh applet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newRootfsBuild(t)
			const previous = "the image of an earlier build"
			if err := os.WriteFile(b.out, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := b.run(tc.env...)
			requireFailure(t, out, err)
			if !strings.Contains(out, tc.message) {
				t.Fatalf("output lacks %q:\n%s", tc.message, out)
			}
			got, readErr := os.ReadFile(b.out)
			if readErr != nil {
				t.Fatalf("the earlier output is gone: %v", readErr)
			}
			if string(got) != previous {
				t.Fatalf("the earlier output changed to %q", got)
			}
			b.requireNoScratch()
		})
	}
}

func TestFailedAppletListingCreatesNoOutput(t *testing.T) {
	b := newRootfsBuild(t)
	out, err := b.run("STUB_APPLETS=sh", "STUB_LIST_STATUS=42")
	requireFailure(t, out, err)
	if _, statErr := os.Stat(b.out); !os.IsNotExist(statErr) {
		t.Fatalf("the failed build left an output file: %v", statErr)
	}
}

func TestWorkingAppletListingBuildsTheImage(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, statErr := os.Stat("/usr/sbin/mkfs.ext4"); statErr != nil {
			t.Skip("mkfs.ext4 is not available")
		}
	}
	b := newRootfsBuild(t)
	out, err := b.run("STUB_APPLETS=sh\nls\nbusybox\ninit")
	if err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}
	info, statErr := os.Stat(b.out)
	if statErr != nil {
		t.Fatalf("the build wrote no image: %v", statErr)
	}
	if info.Size() != 8<<20 {
		t.Fatalf("image size = %d, want %d", info.Size(), 8<<20)
	}
	b.requireNoScratch()
}

func TestBusyboxLinkageCheck(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, statErr := os.Stat("/usr/sbin/mkfs.ext4"); statErr != nil {
			t.Skip("mkfs.ext4 is not available")
		}
	}
	const applets = "STUB_APPLETS=sh\nls\nbusybox\ninit"
	const dynamic = "dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2"
	cases := []struct {
		name          string
		description   string
		pathDirectory string
		link          bool
		dangling      bool
		wantBuild     bool
	}{
		{name: "static", description: "statically linked", wantBuild: true},
		{name: "static-pie", description: "static-pie linked", wantBuild: true},
		{name: "symlink to static", description: "statically linked", link: true, wantBuild: true},
		{name: "symlink to static-pie", description: "static-pie linked", link: true, wantBuild: true},
		{name: "dynamic", description: dynamic},
		{name: "dynamic with static text in directory", description: dynamic, pathDirectory: "statically linked"},
		{name: "symlink to dynamic", description: dynamic, link: true},
		{name: "dangling symlink", description: "statically linked", link: true, dangling: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newRootfsBuild(t)
			busybox := filepath.Join(b.dir, "bin", "busybox")
			if tc.pathDirectory != "" {
				dir := filepath.Join(b.dir, tc.pathDirectory)
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				input := filepath.Join(dir, "busybox")
				if err := os.Rename(busybox, input); err != nil {
					t.Fatal(err)
				}
				busybox = input
			}
			if tc.link {
				target := busybox
				if tc.dangling {
					target = filepath.Join(b.dir, "missing-busybox")
				}
				link := filepath.Join(b.dir, "busybox-link")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				busybox = link
			}
			out, err := b.run(applets, "BUSYBOX="+busybox, "STUB_FILE_DESCRIPTION="+tc.description)
			if tc.wantBuild {
				if err != nil {
					t.Fatalf("the build failed: %v\n%s", err, out)
				}
				if _, statErr := os.Stat(b.out); statErr != nil {
					t.Fatalf("the build wrote no image: %v", statErr)
				}
				return
			}
			requireFailure(t, out, err)
			if !strings.Contains(out, "busybox must be statically linked") {
				t.Fatalf("output lacks the linkage message:\n%s", out)
			}
			if _, statErr := os.Stat(b.out); !os.IsNotExist(statErr) {
				t.Fatalf("the failed build left an output file: %v", statErr)
			}
		})
	}
}
