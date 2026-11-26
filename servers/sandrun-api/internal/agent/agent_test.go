// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/sandrun-api/internal/integration"
)

const (
	linux   = sandrunv1.GuestOs_GUEST_OS_LINUX
	windows = sandrunv1.GuestOs_GUEST_OS_WINDOWS
)

func TestPrepareOrgDeletionKeepsOtherAttemptStopped(t *testing.T) {
	h := &Hub{
		svc:     &api.Service{DB: dbtest.New(t)},
		stopped: map[string]int{},
		syncNow: make(chan struct{}, 1),
	}
	d := api.OrganizationDeletion{OrganizationID: api.NewID()}
	if err := h.prepareOrgDeletion(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.prepareOrgDeletion(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare error = %v, want context canceled", err)
	}
	if count := h.stopped[d.OrganizationID]; count != 1 {
		t.Fatalf("finished attempt affected the remaining stop, count = %d", count)
	}
	select {
	case <-h.syncNow:
		t.Fatal("watcher sync was scheduled while another attempt holds the stop")
	default:
	}
	h.resumeOrg(d.OrganizationID)
	if _, stopped := h.stopped[d.OrganizationID]; stopped {
		t.Fatal("organization stays stopped after both attempts finished")
	}
	select {
	case <-h.syncNow:
	default:
		t.Fatal("watcher sync is not scheduled")
	}
}

func TestPrepareOrgDeletionJoinsLeaseDrain(t *testing.T) {
	orgID := api.NewID()
	watchCtx, cancelWatch := context.WithCancel(t.Context())
	defer cancelWatch()
	w := &watcher{
		in:     integration.Integration{ID: api.NewID(), OrganizationID: orgID},
		cancel: cancelWatch,
		done:   make(chan struct{}),
	}
	h := &Hub{
		svc:      &api.Service{DB: dbtest.New(t)},
		log:      slog.Default(),
		timing:   DefaultTiming(),
		watchers: map[string]*watcher{w.in.ID: w},
		draining: map[string]map[*watcher]struct{}{},
		stopped:  map[string]int{},
	}
	release := sync.OnceFunc(func() { h.finishWatcher(w) })
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	leaseCtx, cancelLease := context.WithCancel(ctx)
	cancelLease()
	leaseDone := make(chan error, 1)
	go func() { leaseDone <- h.runWatchers(leaseCtx) }()
	select {
	case <-watchCtx.Done():
	case <-ctx.Done():
		t.Fatal("lease shutdown did not cancel the watcher")
	}

	prepared := make(chan error, 1)
	go func() { prepared <- h.prepareOrgDeletion(ctx, api.OrganizationDeletion{OrganizationID: orgID}) }()
	select {
	case err := <-prepared:
		t.Fatalf("deletion preparation returned while the lease watcher was draining: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatalf("deletion preparation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("deletion preparation did not finish after the watcher exited")
	}
	select {
	case err := <-leaseDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lease watcher loop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the lease watcher loop did not stop")
	}
}

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
	p := Program{Dir: dir, CredentialEnv: "SANDRUN_TEST_KEY", Env: [][2]string{{"SANDRUN_TEST_WORKER", "sandrun-1"}},
		Args: []string{"/bin/sh", "-c", `printf '%s|%s|%s|%s|%s' "$SANDRUN_TEST_KEY" "$SANDRUN_TEST_WORKER" "$1" "$(pwd)" "$3"`, "x", "arg 'with' quotes", HomeArg,
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
	if string(log) != "s3cr'et value|sandrun-1|arg 'with' quotes|"+home+"|"+home+"/sub dir" {
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
		Dir:           `C:\ProgramData\sandrun\runners\r1`,
		CredentialEnv: "CURSOR_API_KEY",
		Env:           [][2]string{{"CURSOR_AGENT_WORKER_ID", "sandrun-1"}, {"SANDRUN_TEST_LABEL", "O’Brien"}},
		Args:          []string{"agent", "worker", "--pool", "it's", "--worker-dir", HomeArg, "--base", HomeArg + `\w`, "Bob’s pool", "start"},
	})
	for _, want := range []string{
		`$env:CURSOR_API_KEY = [IO.File]::ReadAllText('C:\ProgramData\sandrun\runners\r1\credential')`,
		`Remove-Item -Force -LiteralPath 'C:\ProgramData\sandrun\runners\r1\credential'`,
		`$env:CURSOR_AGENT_WORKER_ID = 'sandrun-1'`,
		`$env:SANDRUN_TEST_LABEL = 'O’’Brien'`,
		`& 'agent' 'worker' '--pool' 'it''s' '--worker-dir' $env:USERPROFILE '--base' ($env:USERPROFILE + '\w') 'Bob’’s pool' 'start' *>> 'C:\ProgramData\sandrun\runners\r1\runner.log'`,
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
	if end, _ := a.ProgramExited(nil, &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero}); end.State != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		end.Reason != sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("exit 0: %+v", end)
	}
	for _, res := range []*ownsand.ExecResult{{Kind: ownsand.ExecResultExited, ExitCode: &one}, {Kind: ownsand.ExecResultFailed}, {Kind: ownsand.ExecResultExited, ExitCode: &zero, TimedOut: true}} {
		if end, _ := a.ProgramExited(nil, res); end.State != sandrunv1.RequestState_REQUEST_STATE_FAILED || end.Reason != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST {
			t.Fatalf("%+v: %+v", res, end)
		}
	}
}
