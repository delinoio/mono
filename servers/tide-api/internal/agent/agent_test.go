// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

const (
	linux   = tidev1.GuestOs_GUEST_OS_LINUX
	windows = tidev1.GuestOs_GUEST_OS_WINDOWS
)

func TestQuote(t *testing.T) {
	if got := Quote(linux, "a'b $x"); got != `'a'\''b $x'` {
		t.Fatalf("posix %s", got)
	}
	if got := Quote(linux, "a’b"); got != "'a’b'" {
		t.Fatalf("POSIX quote = %q, want %q", got, "'a’b'")
	}
	for _, tt := range []struct {
		name  string
		value string
		want  string
	}{
		{"ascii", "a'b $x", "'a''b $x'"},
		{"left", "a‘b", "'a‘‘b'"},
		{"right", "a’b", "'a’’b'"},
		{"low", "a‚b", "'a‚‚b'"},
		{"reversed", "a‛b", "'a‛‛b'"},
		{"mixed", "O'Neil’s ‘workspace’", "'O''Neil’’s ‘‘workspace’’'"},
		{"path", `C:\Users\O’Brien\ws`, `'C:\Users\O’’Brien\ws'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(windows, tt.value); got != tt.want {
				t.Fatalf("PowerShell quote = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuoteInMakeDirScript(t *testing.T) {
	script := makeDirScript(windows, `C:\Users\O’Brien\runner`)
	want := `$d = 'C:\Users\O’’Brien\runner'` + "\n"
	if !strings.Contains(script, want) {
		t.Fatalf("script lacks %q:\n%s", want, script)
	}
}

// TestStartScriptPOSIX runs the start script with /bin/sh: the program gets
// the credential in its environment, the credential file is gone, the
// arguments arrive unchanged and the output goes to the log file.
func TestStartScriptPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	dir := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialFile), []byte("s3cr'et value"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Program{Dir: dir, CredentialEnv: "TIDE_TEST_KEY", Env: [][2]string{{"TIDE_TEST_WORKER", "tide-1"}},
		Args: []string{"/bin/sh", "-c", `printf '%s|%s|%s|%s|%s' "$TIDE_TEST_KEY" "$TIDE_TEST_WORKER" "$1" "$(pwd)" "$3"`, "x", "arg 'with' quotes", HomeArg,
			HomeArg + "/sub dir"}}
	script := StartScript(linux, p)
	if strings.Contains(script, "s3cr") {
		t.Fatal("the script contains the credential")
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	log, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(log) != "s3cr'et value|tide-1|arg 'with' quotes|"+home+"|"+home+"/sub dir" {
		t.Fatalf("log %q", log)
	}
	if _, err := os.Stat(filepath.Join(dir, CredentialFile)); !os.IsNotExist(err) {
		t.Fatalf("credential file still exists: %v", err)
	}
	// A missing credential file stops the start with exit code 20.
	cmd = exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 20 {
		t.Fatalf("missing credential: %v", err)
	}
}

func TestStartScriptWindows(t *testing.T) {
	s := StartScript(windows, Program{
		Dir:           `C:\ProgramData\tide\runners\r1`,
		CredentialEnv: "CURSOR_API_KEY",
		Env:           [][2]string{{"CURSOR_AGENT_WORKER_ID", "tide-1"}, {"TIDE_TEST_LABEL", "O’Brien"}},
		Args:          []string{"agent", "worker", "--pool", "it's", "--worker-dir", HomeArg, "--base", HomeArg + `\w`, "Bob’s pool", "start"},
	})
	for _, want := range []string{
		`$env:CURSOR_API_KEY = [IO.File]::ReadAllText('C:\ProgramData\tide\runners\r1\credential')`,
		`Remove-Item -Force -LiteralPath 'C:\ProgramData\tide\runners\r1\credential'`,
		`$env:CURSOR_AGENT_WORKER_ID = 'tide-1'`,
		`$env:TIDE_TEST_LABEL = 'O’’Brien'`,
		`& 'agent' 'worker' '--pool' 'it''s' '--worker-dir' $env:USERPROFILE '--base' ($env:USERPROFILE + '\w') 'Bob’’s pool' 'start' *>> 'C:\ProgramData\tide\runners\r1\runner.log'`,
		"exit $LASTEXITCODE",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestProgramExited(t *testing.T) {
	zero, one := int64(0), int64(1)
	a := &Adapter{}
	if end, _ := a.ProgramExited(nil, &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero}); end.State != tidev1.RequestState_REQUEST_STATE_ENDED ||
		end.Reason != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("exit 0: %+v", end)
	}
	for _, res := range []*ownsand.ExecResult{{Kind: ownsand.ExecResultExited, ExitCode: &one}, {Kind: ownsand.ExecResultFailed}, {Kind: ownsand.ExecResultExited, ExitCode: &zero, TimedOut: true}} {
		if end, _ := a.ProgramExited(nil, res); end.State != tidev1.RequestState_REQUEST_STATE_FAILED || end.Reason != tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST {
			t.Fatalf("%+v: %+v", res, end)
		}
	}
}
