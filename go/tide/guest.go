// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// The guest commands of sync and download. Every command gets its paths
// and options through environment variables of the exec request, never
// inside the command text, so that no path needs quoting for a shell. The
// docs/packages-tide-sdk-foundation.md contract records these commands.
//
// Linux and macOS guests run POSIX sh scripts with `sh -c <script> tide`.
// Windows guests run Windows PowerShell 5.1 scripts with
// `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass
// -EncodedCommand <base64 UTF-16LE script>`, because the encoded command
// contains only base64 characters, so it survives every Windows command line
// quoting rule, and use the tar.exe of Windows 10 and later.

// Environment variable names of the guest commands.
const (
	guestEnvLock          = "TIDE_SYNC_LOCK"
	guestEnvForce         = "TIDE_SYNC_FORCE"
	guestEnvWorkspace     = "TIDE_SYNC_WORKSPACE"
	guestEnvStaleManifest = "TIDE_SYNC_STALE_MANIFEST"
	guestEnvArchive       = "TIDE_SYNC_ARCHIVE"
	guestEnvDeletes       = "TIDE_SYNC_DELETE_LIST"
	guestEnvReplaceFiles  = "TIDE_SYNC_REPLACE_FILE_LIST"
	guestEnvPrunes        = "TIDE_SYNC_PRUNE_LIST"
	guestEnvLinks         = "TIDE_SYNC_LINK_LIST"
	guestEnvPath          = "TIDE_DOWNLOAD_PATH"
	guestEnvDLArchive     = "TIDE_DOWNLOAD_ARCHIVE"
)

// exitLocked is the exit code of the lock command when the lock exists.
const exitLocked = 75

// exitWorkspaceMissing means the lock was acquired but the workspace is missing.
const exitWorkspaceMissing = 76

// exitNotFound is the exit code of the download probe for a missing path.
const exitNotFound = 66

// linkFailedPrefix starts each stdout line of the Windows apply command for
// a symbolic link that the guest could not create.
const linkFailedPrefix = "TIDE-LINK-FAILED "

// posixScripts are the sh scripts of Linux and macOS guests.
var posixScripts = map[guestOp]string{
	opLock: `if [ "$TIDE_SYNC_FORCE" = 1 ]; then rm -rf "$TIDE_SYNC_LOCK" || exit 1; fi
if mkdir "$TIDE_SYNC_LOCK"; then
  if [ -d "$TIDE_SYNC_WORKSPACE" ]; then exit 0; fi
  exit 76
fi
if [ -e "$TIDE_SYNC_LOCK" ]; then exit 75; fi
exit 1`,
	opApply: `set -e
if [ -n "$TIDE_SYNC_STALE_MANIFEST" ]; then rm -f "$TIDE_SYNC_STALE_MANIFEST"; fi
mkdir -p "$TIDE_SYNC_WORKSPACE"
cd "$TIDE_SYNC_WORKSPACE"
if [ -n "$TIDE_SYNC_DELETE_LIST" ]; then xargs -0 rm -f -- < "$TIDE_SYNC_DELETE_LIST"; fi
if [ -n "$TIDE_SYNC_REPLACE_FILE_LIST" ]; then
  xargs -0 sh -c 'for p do
    if [ ! -L "$p" ] && [ -d "$p" ]; then continue; fi
    rm -f -- "$p" || exit 1
  done' tide < "$TIDE_SYNC_REPLACE_FILE_LIST"
fi
if [ -n "$TIDE_SYNC_PRUNE_LIST" ]; then xargs -0 sh -c 'for d do if [ -d "$d" ] && [ ! -L "$d" ]; then rmdir -- "$d" || exit 1; fi; done' tide < "$TIDE_SYNC_PRUNE_LIST"; fi
if [ -n "$TIDE_SYNC_ARCHIVE" ]; then tar -x -m -o -f "$TIDE_SYNC_ARCHIVE"; fi`,
	opCleanup: `status=0
for f in "$TIDE_SYNC_ARCHIVE" "$TIDE_SYNC_DELETE_LIST" "$TIDE_SYNC_REPLACE_FILE_LIST" "$TIDE_SYNC_PRUNE_LIST"; do
  if [ -n "$f" ]; then rm -f "$f" || status=1; fi
done
if [ -n "$TIDE_SYNC_LOCK" ]; then rm -rf "$TIDE_SYNC_LOCK" || status=1; fi
exit "$status"`,
	opProbe: `p="$TIDE_DOWNLOAD_PATH"
if [ -d "$p" ]; then
  tar -c -f "$TIDE_DOWNLOAD_ARCHIVE" -C "$p" . || exit 1
  printf 'dir %s\n' "$(wc -c < "$TIDE_DOWNLOAD_ARCHIVE" | tr -d ' ')"
elif [ -f "$p" ]; then
  printf 'file %s\n' "$(wc -c < "$p" | tr -d ' ')"
else
  exit 66
fi`,
	opRemoveArchive: `rm -f "$TIDE_DOWNLOAD_ARCHIVE"`,
}

// windowsScripts are the PowerShell scripts of Windows guests.
var windowsScripts = map[guestOp]string{
	// cmd.exe mkdir calls CreateDirectoryW, which fails when the directory
	// exists, so the check and the creation are one step. The
	// %TIDE_SYNC_LOCK% reference is expanded by cmd.exe inside quotes.
	// Disable command extensions so mkdir cannot create missing parents.
	opLock: `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$l = $env:TIDE_SYNC_LOCK
if ($env:TIDE_SYNC_FORCE -eq '1' -and (Test-Path -LiteralPath $l)) { Remove-Item -LiteralPath $l -Recurse -Force }
& cmd.exe /d /e:off /c 'mkdir "%TIDE_SYNC_LOCK%" 2>nul'
if ($LASTEXITCODE -eq 0) {
  if (Test-Path -LiteralPath $env:TIDE_SYNC_WORKSPACE -PathType Container) { exit 0 }
  exit 76
}
if (Test-Path -LiteralPath $l) { exit 75 }
exit 1`,
	opApply: `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$ws = $env:TIDE_SYNC_WORKSPACE
if ($env:TIDE_SYNC_STALE_MANIFEST) { [System.IO.File]::Delete($env:TIDE_SYNC_STALE_MANIFEST) }
[System.IO.Directory]::CreateDirectory($ws) | Out-Null
if ($env:TIDE_SYNC_DELETE_LIST) {
  foreach ($p in [System.IO.File]::ReadAllLines($env:TIDE_SYNC_DELETE_LIST, [System.Text.Encoding]::UTF8)) {
    if ($p -eq '') { continue }
    $t = Join-Path $ws $p
    if (Test-Path -LiteralPath $t) { Remove-Item -LiteralPath $t -Force }
  }
}
if ($env:TIDE_SYNC_PRUNE_LIST) {
  foreach ($p in [System.IO.File]::ReadAllLines($env:TIDE_SYNC_PRUNE_LIST, [System.Text.Encoding]::UTF8)) {
    if ($p -eq '') { continue }
    $t = Join-Path $ws $p
    if (Test-Path -LiteralPath $t -PathType Container) { [System.IO.Directory]::Delete($t) }
  }
}
if ($env:TIDE_SYNC_ARCHIVE) {
  & "$env:SystemRoot\System32\tar.exe" -x -m -f $env:TIDE_SYNC_ARCHIVE -C $ws
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
if ($env:TIDE_SYNC_LINK_LIST) {
  foreach ($line in [System.IO.File]::ReadAllLines($env:TIDE_SYNC_LINK_LIST, [System.Text.Encoding]::UTF8)) {
    if ($line -eq '') { continue }
    $parts = $line.Split([char]9, 2)
    $t = Join-Path $ws $parts[0]
    try {
      [System.IO.Directory]::CreateDirectory((Split-Path -Parent $t)) | Out-Null
      if (Test-Path -LiteralPath $t) { Remove-Item -LiteralPath $t -Force }
      New-Item -ItemType SymbolicLink -Path $t -Value $parts[1] | Out-Null
    } catch {
      [Console]::Out.WriteLine('TIDE-LINK-FAILED ' + $parts[0])
    }
  }
}
exit 0`,
	opCleanup: `$ProgressPreference = 'SilentlyContinue'
$status = 0
foreach ($f in @($env:TIDE_SYNC_ARCHIVE, $env:TIDE_SYNC_DELETE_LIST, $env:TIDE_SYNC_PRUNE_LIST, $env:TIDE_SYNC_LINK_LIST)) {
  try {
    if ($f -and (Test-Path -LiteralPath $f -ErrorAction Stop)) {
      Remove-Item -LiteralPath $f -Force -ErrorAction Stop
    }
  } catch {
    $status = 1
    Write-Error -ErrorRecord $_ -ErrorAction Continue
  }
}
$l = $env:TIDE_SYNC_LOCK
try {
  if ($l -and (Test-Path -LiteralPath $l -ErrorAction Stop)) {
    Remove-Item -LiteralPath $l -Recurse -Force -ErrorAction Stop
  }
} catch {
  $status = 1
  Write-Error -ErrorRecord $_ -ErrorAction Continue
}
exit $status`,
	opProbe: `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$p = $env:TIDE_DOWNLOAD_PATH
if (Test-Path -LiteralPath $p -PathType Container) {
  & "$env:SystemRoot\System32\tar.exe" -c -f $env:TIDE_DOWNLOAD_ARCHIVE -C $p .
  if ($LASTEXITCODE -ne 0) { exit 1 }
  [Console]::Out.WriteLine('dir ' + (Get-Item -LiteralPath $env:TIDE_DOWNLOAD_ARCHIVE).Length)
} elseif (Test-Path -LiteralPath $p -PathType Leaf) {
  [Console]::Out.WriteLine('file ' + (Get-Item -LiteralPath $p).Length)
} else {
  exit 66
}`,
	opRemoveArchive: `$ProgressPreference = 'SilentlyContinue'
$f = $env:TIDE_DOWNLOAD_ARCHIVE
try {
  if ($f -and (Test-Path -LiteralPath $f -ErrorAction Stop)) {
    Remove-Item -LiteralPath $f -Force -ErrorAction Stop
  }
} catch {
  Write-Error -ErrorRecord $_ -ErrorAction Continue
  exit 1
}
exit 0`,
}

// guestOp names one guest command.
type guestOp int

const (
	opLock guestOp = iota + 1
	opApply
	opCleanup
	opProbe
	opRemoveArchive
)

func (op guestOp) String() string {
	switch op {
	case opLock:
		return "lock"
	case opApply:
		return "apply"
	case opCleanup:
		return "cleanup"
	case opProbe:
		return "probe"
	case opRemoveArchive:
		return "remove-archive"
	}
	return fmt.Sprintf("guestOp(%d)", int(op))
}

// guestCommand returns the argument array of a guest command for os.
func guestCommand(os tidev1.GuestOs, op guestOp) ([]string, error) {
	switch os {
	case tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS:
		return []string{"sh", "-c", posixScripts[op], "tide"}, nil
	case tidev1.GuestOs_GUEST_OS_WINDOWS:
		return []string{
			"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-EncodedCommand", encodePowerShell(windowsScripts[op]),
		}, nil
	}
	return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("tide: unsupported guest OS %s", os))
}

// encodePowerShell returns the -EncodedCommand form of script: base64 of
// its UTF-16LE encoding.
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, r := range u {
		b = append(b, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// guestResult is the captured result of a guest command.
type guestResult struct {
	exitCode int64
	// stdout and stderr hold at most the first guestOutputLimit bytes.
	stdout []byte
	stderr []byte
	// linkFailures lists the paths of all linkFailedPrefix lines of
	// stdout, also those after the captured start of stdout.
	linkFailures []string
	// outputLost is true when linkFailures can be incomplete: the exec
	// lost output, or a stdout line was longer than guestOutputLimit.
	outputLost bool
}

// guestOutputLimit bounds the captured output of a guest command and the
// length of one stdout line that the link failure scan reads.
const guestOutputLimit = 64 << 10

// linkFailureScan finds the linkFailedPrefix lines in a stdout stream
// that arrives in chunks.
type linkFailureScan struct {
	line  []byte
	long  bool // the current line is longer than guestOutputLimit
	lost  bool
	paths []string
}

func (s *linkFailureScan) write(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !s.long {
			if len(s.line)+len(part) > guestOutputLimit {
				s.long, s.lost, s.line = true, true, s.line[:0]
			} else {
				s.line = append(s.line, part...)
			}
		}
		if i < 0 {
			return
		}
		s.end()
		b = b[i+1:]
	}
}

// end ends the current line.
func (s *linkFailureScan) end() {
	if !s.long {
		line := strings.TrimRight(string(s.line), "\r")
		if rel, ok := strings.CutPrefix(line, linkFailedPrefix); ok {
			s.paths = append(s.paths, rel)
		}
	}
	s.line, s.long = s.line[:0], false
}

// guestRoot returns a working directory that is independent of the workspace.
func guestRoot(s *tidev1.DevSession) string {
	switch s.GetGuestOs() {
	case tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS:
		return "/"
	case tidev1.GuestOs_GUEST_OS_WINDOWS:
		p := s.GetWorkspacePath()
		if len(p) >= 3 && isASCIILetter(p[0]) && p[1:3] == `:\` {
			return p[:3]
		}
	}
	return ""
}

// runGuest runs one guest command through Exec and captures its output. A
// command that cannot start, that a signal ends or whose sandbox ends is an
// error.
func (c *Client) runGuest(ctx context.Context, s *tidev1.DevSession, op guestOp, env map[string]string) (*guestResult, error) {
	cmd, err := guestCommand(s.GetGuestOs(), op)
	if err != nil {
		return nil, err
	}
	if s.GetGuestOs() == tidev1.GuestOs_GUEST_OS_MACOS {
		// Keep macOS tar from adding AppleDouble (._) files.
		env["COPYFILE_DISABLE"] = "1"
	}
	var out, errb bytes.Buffer
	var scan linkFailureScan
	res, err := c.Exec(ctx, ExecParams{
		OrganizationID:   s.GetOrganizationId(),
		DevSessionID:     s.GetDevSessionId(),
		Command:          cmd,
		WorkingDirectory: guestRoot(s),
		Env:              env,
		OnEvent: func(ev ExecEvent) error {
			switch ev.Kind {
			case ExecEventStdout:
				if out.Len() < guestOutputLimit {
					out.Write(ev.Data)
				}
				scan.write(ev.Data)
			case ExecEventStderr:
				if errb.Len() < guestOutputLimit {
					errb.Write(ev.Data)
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	c.logger.DebugContext(ctx, "tide guest command ended", "op", op.String(), "exec_id", res.ExecID, "result", res.Kind.String())
	switch res.Kind {
	case ExecResultFailed:
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("tide: the guest %s command could not start (%s): %s", op, enumText(res.FailureReason), res.FailureMessage))
	case ExecResultSandboxEnded:
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("tide: the dev session ended during the guest %s command (%s)", op, res.SandboxEndReason))
	}
	if res.Signal != nil {
		return nil, localError(connect.CodeInternal, fmt.Errorf("tide: the guest %s command ended with signal %d", op, *res.Signal))
	}
	scan.end()
	r := &guestResult{stdout: out.Bytes(), stderr: errb.Bytes(), linkFailures: scan.paths, outputLost: scan.lost || res.Incomplete}
	if res.ExitCode != nil {
		r.exitCode = *res.ExitCode
	}
	return r, nil
}

// failure returns the error of a guest command that exited with a nonzero
// code. The message carries the end of its standard error, which describes
// guest paths only, never file contents.
func (r *guestResult) failure(op guestOp) error {
	msg := strings.TrimSpace(string(r.stderr))
	if len(msg) > 2000 {
		msg = "..." + msg[len(msg)-2000:]
	}
	if msg == "" {
		return localError(connect.CodeInternal, fmt.Errorf("tide: the guest %s command exited with code %d", op, r.exitCode))
	}
	return localError(connect.CodeInternal, fmt.Errorf("tide: the guest %s command exited with code %d: %s", op, r.exitCode, msg))
}

func enumText(e interface{ String() string }) string { return strings.ToLower(e.String()) }

// Guest paths.

// guestSep returns the path separator of os.
func guestSep(os tidev1.GuestOs) string {
	if os == tidev1.GuestOs_GUEST_OS_WINDOWS {
		return `\`
	}
	return "/"
}

// guestIsAbs reports whether p is an absolute path of os.
func guestIsAbs(os tidev1.GuestOs, p string) bool {
	if os == tidev1.GuestOs_GUEST_OS_WINDOWS {
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return true
		}
		return len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
	}
	return strings.HasPrefix(p, "/")
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// guestTrim removes trailing separators of p, but keeps a root.
func guestTrim(os tidev1.GuestOs, p string) string {
	cut := "/"
	if os == tidev1.GuestOs_GUEST_OS_WINDOWS {
		cut = `\/`
	}
	t := strings.TrimRight(p, cut)
	if t == "" || (os == tidev1.GuestOs_GUEST_OS_WINDOWS && len(t) == 2 && t[1] == ':') {
		return p
	}
	return t
}

// guestJoin joins the workspace path and a slash-separated relative path.
func guestJoin(os tidev1.GuestOs, base, rel string) string {
	base = guestTrim(os, base)
	rel = strings.TrimLeft(rel, "/")
	if os == tidev1.GuestOs_GUEST_OS_WINDOWS {
		rel = strings.ReplaceAll(rel, "/", `\`)
	}
	if rel == "" || rel == "." {
		return base
	}
	if strings.HasSuffix(base, guestSep(os)) {
		return base + rel
	}
	return base + guestSep(os) + rel
}

// guestResolve returns p as an absolute guest path: p itself when it is
// absolute, otherwise p relative to the workspace path.
func guestResolve(s *tidev1.DevSession, p string) string {
	if guestIsAbs(s.GetGuestOs(), p) {
		return p
	}
	return guestJoin(s.GetGuestOs(), s.GetWorkspacePath(), p)
}

// guestSibling returns the path next to the workspace: the workspace path
// without trailing separators followed by suffix.
func guestSibling(s *tidev1.DevSession, suffix string) string {
	return guestTrim(s.GetGuestOs(), s.GetWorkspacePath()) + suffix
}

// errNoWorkspace reports a dev session without a workspace path.
var errNoWorkspace = errors.New("tide: the dev session has no workspace path")
