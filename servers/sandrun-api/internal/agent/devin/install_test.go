// SPDX-License-Identifier: FSL-1.1-ALv2

package devin

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestInstallScriptPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	for _, tc := range []struct {
		name           string
		downloadCode   int
		installer      string
		preinstalled   bool
		wantCode       int
		wantBashCalled bool
	}{
		{name: "failed download", downloadCode: 22, wantCode: 12},
		{name: "partial download", downloadCode: 18, installer: "exit 0", wantCode: 12},
		{name: "failed installer", installer: "exit 5", wantCode: 12, wantBashCalled: true},
		{name: "missing CLI", installer: "exit 0", wantCode: 13, wantBashCalled: true},
		{name: "installed CLI", installer: `printf '#!/bin/sh\nexit 0\n' > "$HOME/.devin/bin/devin"
chmod +x "$HOME/.devin/bin/devin"`, wantBashCalled: true},
		{name: "preinstalled CLI", preinstalled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".devin", "bin")
			tmp := filepath.Join(home, "installer files")
			for _, dir := range []string{bin, tmp} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("git", "exit 0")
			stub("uname", "printf 'Linux\\n'")
			stub("curl", `: > "$HOME/curl.called"
if [ "${3:-}" = -o ]; then
  printf '%s\n' "$INSTALLER" > "$4"
else
  printf '%s\n' "$INSTALLER"
fi
exit "$DOWNLOAD_CODE"`)
			stub("bash", `: > "$HOME/bash.called"
exec /bin/sh "$@"`)
			if tc.preinstalled {
				stub("devin", "exit 0")
			}
			for _, name := range []string{"mktemp", "rm", "chmod"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command("/bin/sh", "-c", InstallScript(sandrunv1.GuestOs_GUEST_OS_LINUX))
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + bin,
				"TMPDIR=" + tmp,
				"INSTALLER=" + tc.installer,
				"DOWNLOAD_CODE=" + strconv.Itoa(tc.downloadCode),
			}
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			code := 0
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d; output: %s", code, tc.wantCode, out)
			}
			for name, want := range map[string]bool{"curl": !tc.preinstalled, "bash": tc.wantBashCalled} {
				_, err := os.Stat(filepath.Join(home, name+".called"))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if called := err == nil; called != want {
					t.Errorf("%s called = %t, want %t", name, called, want)
				}
			}
			files, err := os.ReadDir(tmp)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Errorf("temporary files remain: %v", files)
			}
		})
	}
}
