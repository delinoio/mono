// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// Guest file names in the runner directory.
const (
	// CredentialFile holds the runner credential until the runner program's
	// start script reads it into the program's environment and deletes it.
	CredentialFile = "credential"
	// LogFile receives the runner program's stdout and stderr.
	LogFile = "runner.log"
)

// stepTimeout bounds one setup exec (directory, credential checks).
const stepTimeout = 2 * time.Minute

// InstallTimeout bounds the exec that installs a runner program.
const InstallTimeout = 15 * time.Minute

// IsWindows reports whether a guest OS uses PowerShell scripts.
func IsWindows(g tidev1.GuestOs) bool { return g == tidev1.GuestOs_GUEST_OS_WINDOWS }

// Command returns the argument array that runs script: `/bin/sh -c` on
// Linux and macOS, Windows PowerShell on Windows (part of every supported
// Windows guest).
func Command(guest tidev1.GuestOs, script string) []string {
	if IsWindows(guest) {
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}
	}
	return []string{"/bin/sh", "-c", script}
}

// Quote quotes s as one literal word of the guest's script language: a
// POSIX single-quoted string, or a PowerShell single-quoted string.
func Quote(guest tidev1.GuestOs, s string) string {
	if IsWindows(guest) {
		return work.QuotePowerShell(s)
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RunnerDir returns the guest directory of a runner: it holds the
// credential file and the runner log, and only the guest user that runs
// the execs can read it.
func RunnerDir(guest tidev1.GuestOs, runnerID string) string {
	if IsWindows(guest) {
		return `C:\ProgramData\tide\runners\` + runnerID
	}
	return "/tmp/tide-runner-" + runnerID
}

// JoinPath joins a runner directory and a file name.
func JoinPath(guest tidev1.GuestOs, dir, name string) string {
	if IsWindows(guest) {
		return dir + `\` + name
	}
	return dir + "/" + name
}

// posixHome makes $HOME and the user's local bin directories usable in a
// POSIX script: the guest helper may start execs without HOME, and the
// vendor installers put their programs in user directories that the exec
// PATH may not contain (NOT DOCUMENTED by the vendors; see the contract).
func posixHome(extraBins ...string) string {
	s := `[ -n "${HOME:-}" ] || HOME=$(cd ~ 2>/dev/null && pwd) || HOME=/
export HOME
export PATH="$HOME/.local/bin`
	for _, b := range extraBins {
		s += ":$HOME/" + b
	}
	return s + `:$PATH"
`
}

// windowsPath reloads PATH from the machine and user environment, so a
// program that an installer added to the user PATH in an earlier exec is
// found.
const windowsPath = `$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
`

// ScriptPrelude returns the lines that every vendor script starts with:
// HOME and PATH on Linux and macOS (with extra bin directories relative to
// $HOME), PATH on Windows.
func ScriptPrelude(guest tidev1.GuestOs, extraBins ...string) string {
	if IsWindows(guest) {
		return windowsPath
	}
	return posixHome(extraBins...)
}

// PrependPath returns a line that puts bin directories relative to $HOME
// before every other PATH entry, including $HOME/.local/bin, on Linux and
// macOS (nothing on Windows). Use it after ScriptPrelude for a runtime that
// tide installs and that must win over an older one of the guest.
func PrependPath(guest tidev1.GuestOs, bins ...string) string {
	if IsWindows(guest) || len(bins) == 0 {
		return ""
	}
	s := `export PATH="`
	for _, b := range bins {
		s += "$HOME/" + b + ":"
	}
	return s + `$PATH"
`
}

// StepError describes a failed guest step.
type StepError struct {
	Step string
	Code int64
	Msg  string
}

func (e *StepError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s: %s", e.Step, e.Msg)
	}
	return fmt.Sprintf("%s failed with exit code %d", e.Step, e.Code)
}

// RunStep runs a script and waits for it. Only an exit with code 0 before the
// timeout succeeds. Other results return a StepError. The codes map gives
// messages for the step's nonzero exit codes.
func RunStep(ctx context.Context, g *work.Guest, step, script string, timeout time.Duration, codes map[int64]string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	res, err := g.Exec(ctx, ownsand.ExecParams{ExecID: api.NewID(), Command: Command(g.Runner.GuestOS, script), Timeout: timeout})
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	if res.TimedOut {
		return &StepError{Step: step, Code: -1, Msg: "timed out"}
	}
	if res.Kind != ownsand.ExecResultExited || res.ExitCode == nil {
		return &StepError{Step: step, Code: -1, Msg: res.Kind.String()}
	}
	if *res.ExitCode != 0 {
		return &StepError{Step: step, Code: *res.ExitCode, Msg: codes[*res.ExitCode]}
	}
	return nil
}

// makeDirScript creates the runner directory so that only the exec user
// can read it: mode 0700 on Linux and macOS, and on Windows an ACL without
// inheritance that grants only the current user.
func makeDirScript(guest tidev1.GuestOs, dir string) string {
	if IsWindows(guest) {
		return `$ErrorActionPreference = 'Stop'
$d = ` + Quote(guest, dir) + `
New-Item -ItemType Directory -Force -Path $d | Out-Null
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $d /inheritance:r /grant:r "*${sid}:(OI)(CI)F" | Out-Null
if ($LASTEXITCODE -ne 0) { exit 1 }
`
	}
	return "umask 077 && mkdir -p -m 700 " + Quote(guest, dir) + " && chmod 700 " + Quote(guest, dir)
}

// WriteCredential creates the runner directory and writes the runner
// credential to its credential file (mode 0600). The start script reads it
// into the program's environment and deletes it, so after the start only
// the runner process has the value. The credential never appears in an
// exec command or environment that OwnSand sees.
func WriteCredential(ctx context.Context, g *work.Guest, value string) (dir string, err error) {
	guest := g.Runner.GuestOS
	dir = RunnerDir(guest, g.Runner.ID)
	if err := RunStep(ctx, g, "create the runner directory", makeDirScript(guest, dir), stepTimeout, nil); err != nil {
		return "", err
	}
	fw, err := g.Client.OpenFileWrite(g.Runner.SandboxID, ownsand.FileWriteParams{WriteID: api.NewID(), Path: JoinPath(guest, dir, CredentialFile)})
	if err != nil {
		return "", fmt.Errorf("write the runner credential: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	data := []byte(value)
	if _, err := fw.Finish(wctx, 0, data, int64(len(data)), 0o600); err != nil {
		return "", fmt.Errorf("write the runner credential: %w", err)
	}
	return dir, nil
}

// HomeArg is a program argument that the start script replaces with the
// guest user's home directory. An argument that starts with HomeArg is the
// home directory followed by the rest of the argument (for example
// HomeArg + "/claude-workspace").
const HomeArg = "\x00home"

// Program is a runner program start.
type Program struct {
	// Dir is the runner directory of WriteCredential.
	Dir string
	// CredentialEnv is the environment variable that receives the runner
	// credential from the credential file.
	CredentialEnv string
	// Env are more environment variables (no secrets).
	Env [][2]string
	// Args is the program and its arguments; HomeArg is the home directory.
	Args []string
	// ExtraBins are more bin directories relative to $HOME (Linux, macOS).
	ExtraBins []string
	// FirstBins are bin directories relative to $HOME that come before
	// every other PATH entry (Linux, macOS); see PrependPath.
	FirstBins []string
}

// StartScript returns the script that reads the credential file into the
// environment, deletes the file, changes to the home directory and runs
// the program with its output appended to the runner log. Exit code 20
// means that the credential file was missing.
func StartScript(guest tidev1.GuestOs, p Program) string {
	var b strings.Builder
	b.WriteString(ScriptPrelude(guest, p.ExtraBins...))
	b.WriteString(PrependPath(guest, p.FirstBins...))
	cred := JoinPath(guest, p.Dir, CredentialFile)
	log := JoinPath(guest, p.Dir, LogFile)
	if IsWindows(guest) {
		fmt.Fprintf(&b, "try { $env:%s = [IO.File]::ReadAllText(%s) } catch { exit 20 }\n", p.CredentialEnv, Quote(guest, cred))
		fmt.Fprintf(&b, "Remove-Item -Force -LiteralPath %s\n", Quote(guest, cred))
		for _, kv := range p.Env {
			fmt.Fprintf(&b, "$env:%s = %s\n", kv[0], Quote(guest, kv[1]))
		}
		b.WriteString("Set-Location -LiteralPath $env:USERPROFILE\n")
		// Native stderr must not stop the script; it goes to the log.
		b.WriteString("$ErrorActionPreference = 'Continue'\n& ")
		for i, a := range p.Args {
			if i > 0 {
				b.WriteByte(' ')
			}
			if rest, ok := strings.CutPrefix(a, HomeArg); ok {
				if rest != "" {
					b.WriteString("($env:USERPROFILE + " + Quote(guest, rest) + ")")
				} else {
					b.WriteString("$env:USERPROFILE")
				}
			} else {
				b.WriteString(Quote(guest, a))
			}
		}
		fmt.Fprintf(&b, " *>> %s\nexit $LASTEXITCODE\n", Quote(guest, log))
		return b.String()
	}
	fmt.Fprintf(&b, "%s=$(cat %s) || exit 20\nrm -f %s\nexport %s\n", p.CredentialEnv, Quote(guest, cred), Quote(guest, cred), p.CredentialEnv)
	for _, kv := range p.Env {
		fmt.Fprintf(&b, "export %s=%s\n", kv[0], Quote(guest, kv[1]))
	}
	b.WriteString("cd \"$HOME\" || exit 21\nexec")
	for _, a := range p.Args {
		b.WriteByte(' ')
		if rest, ok := strings.CutPrefix(a, HomeArg); ok {
			b.WriteString(`"$HOME"`)
			if rest != "" {
				b.WriteString(Quote(guest, rest))
			}
		} else {
			b.WriteString(Quote(guest, a))
		}
	}
	fmt.Fprintf(&b, " >>%s 2>&1\n", Quote(guest, log))
	return b.String()
}

var errStarted = errors.New("the runner program started")

// StartProgram starts the runner program with an OwnSand exec and returns
// as soon as the guest reports the start; the program keeps running and
// the engine follows the exec with AttachExec. Its output goes to the log
// file, so the exec stream carries only the exit result.
func StartProgram(ctx context.Context, g *work.Guest, p Program) (work.StartResult, error) {
	execID := api.NewID()
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	res, err := g.Exec(ctx, ownsand.ExecParams{ExecID: execID, Command: Command(g.Runner.GuestOS, StartScript(g.Runner.GuestOS, p)),
		OnEvent: func(e ownsand.ExecEvent) error {
			if e.Kind == ownsand.ExecEventStarted {
				return errStarted
			}
			return nil
		}})
	if errors.Is(err, errStarted) {
		return work.StartResult{ProgramExecID: execID}, nil
	}
	if err != nil {
		return work.StartResult{}, fmt.Errorf("start the runner program: %w", err)
	}
	return work.StartResult{}, fmt.Errorf("the runner program ended at once (%s)", describe(res))
}

func describe(res *ownsand.ExecResult) string {
	switch {
	case res == nil:
		return "no result"
	case res.ExitCode != nil:
		return fmt.Sprintf("exit code %d", *res.ExitCode)
	case res.Signal != nil:
		return fmt.Sprintf("signal %d", *res.Signal)
	}
	return res.Kind.String()
}
