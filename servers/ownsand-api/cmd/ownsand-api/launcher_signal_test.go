// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

const (
	// launcherHealthTimeout covers the `go build` step of the launcher (the
	// launcher allows it a long build on a cold cache) plus the server start.
	launcherHealthTimeout = 120 * time.Second
	// launcherDrainTimeout bounds the wait for the launcher to exit after
	// SIGTERM before the test escalates to SIGKILL.
	launcherDrainTimeout = 20 * time.Second
	// launcherKillTimeout bounds the wait for the launcher to be reaped after
	// the teardown sent SIGKILL.
	launcherKillTimeout = 10 * time.Second
	// launcherPipeDrainDelay is how long after the launcher exits that
	// cmd.Wait still waits for descendants to close the inherited output
	// pipes before it closes them itself.
	launcherPipeDrainDelay = 2 * time.Second
	// launcherTrackInterval is how often the fixture records the launcher's
	// descendants for the teardown.
	launcherTrackInterval = 250 * time.Millisecond
	// statusSigterm is 128 + SIGTERM, the status the launcher reports when a
	// forwarded SIGTERM stops the server.
	statusSigterm = 143
)

// lockedBuffer collects the launcher output from two pipes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// launcherProc is a running `node scripts/dev.mjs`.
type launcherProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	output *lockedBuffer
	done   chan struct{}

	mu sync.Mutex
	// seenPIDs and seenPGIDs record every descendant (and its process group)
	// that the tracker or a stop snapshot saw while it was alive. The launcher
	// starts `go build` and the server detached, each in its own process group,
	// and a process that has lost its parent can no longer be found through
	// pgrep -P, so the ids are recorded while the tree is still connected.
	seenPIDs  map[int]struct{}
	seenPGIDs map[int]struct{}
}

// descendantPIDs lists every descendant of pid, found recursively through
// pgrep -P. The launcher runs the build and the server detached, in their own
// process groups, so a SIGKILL of the launcher group does not reach them; the
// test finds them through their parents instead. It is best effort: without
// pgrep it returns nothing.
func descendantPIDs(pid int) []int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		child, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		pids = append(pids, child)
		pids = append(pids, descendantPIDs(child)...)
	}
	return pids
}

// recordDescendants adds the current descendants of the launcher, and their
// process groups, to the recorded sets.
func (p *launcherProc) recordDescendants() {
	pids := descendantPIDs(p.cmd.Process.Pid)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pid := range pids {
		p.seenPIDs[pid] = struct{}{}
		if pgid, err := syscall.Getpgid(pid); err == nil {
			p.seenPGIDs[pgid] = struct{}{}
		}
	}
}

// trackDescendants records the descendants until the launcher is reaped.
func (p *launcherProc) trackDescendants() {
	ticker := time.NewTicker(launcherTrackInterval)
	defer ticker.Stop()
	for {
		p.recordDescendants()
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
	}
}

// newLauncherProc starts cmd as a launcher fixture: in its own process group,
// with its output collected, and with a bounded wait for the output pipes.
//
// The output is not an *os.File, so exec copies it through a pipe, and
// cmd.Wait does not return until every holder of the write end has closed it.
// A detached descendant of the launcher (for example a build tool that
// outlives its leader) inherits that pipe and could block Wait, and with it
// the teardown, for as long as it lives. WaitDelay bounds that wait: Wait
// closes the pipes and returns launcherPipeDrainDelay after the launcher
// exits.
func newLauncherProc(t *testing.T, cmd *exec.Cmd) *launcherProc {
	t.Helper()
	// Own process group, so a failure cleanup can kill the launcher group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &lockedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = launcherPipeDrainDelay
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the launcher: %v", err)
	}
	p := &launcherProc{
		t:         t,
		cmd:       cmd,
		output:    output,
		done:      make(chan struct{}),
		seenPIDs:  map[int]struct{}{},
		seenPGIDs: map[int]struct{}{},
	}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	go p.trackDescendants()
	return p
}

// stop ends the launcher and everything that it started, and waits until the
// launcher is reaped. It first forwards SIGTERM, which drains the server. If
// the launcher is still running after launcherDrainTimeout, or when it has
// already exited, it kills every recorded descendant, every recorded
// descendant process group and the launcher's process group, because an exited
// launcher or build leader can leave a descendant that still holds the output
// pipe. The final wait is bounded: a launcher that is not reaped after the
// kills fails the test instead of hanging it. It is safe to call more than
// once.
func (p *launcherProc) stop() {
	p.recordDescendants()
	select {
	case <-p.done:
	default:
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(launcherDrainTimeout):
		}
	}
	p.killTree()
	select {
	case <-p.done:
	case <-time.After(launcherKillTimeout):
		p.t.Errorf("the launcher was not reaped within %s after SIGKILL:\n%s", launcherKillTimeout, p.output)
	}
}

// killTree sends SIGKILL to every recorded descendant and process group and to
// the launcher group. Process ids are recorded for the duration of one test,
// so the chance that an id was reused by an unrelated process is negligible;
// a process or group that is already gone makes kill fail, which is ignored.
func (p *launcherProc) killTree() {
	p.mu.Lock()
	var pids, pgids []int
	for pid := range p.seenPIDs {
		pids = append(pids, pid)
	}
	for pgid := range p.seenPGIDs {
		pgids = append(pgids, pgid)
	}
	p.mu.Unlock()
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, pgid := range pgids {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startLauncher runs the real Node launcher in the package directory. The
// environment carries the generated database URL as DATABASE_URL and never
// the administration URL, so the launched server cannot reach the
// administration database.
func startLauncher(t *testing.T, databaseURL string, port int) *launcherProc {
	t.Helper()
	packageDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", filepath.Join("scripts", "dev.mjs"))
	cmd.Dir = packageDir
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "DATABASE_URL" || name == dbtest.EnvTestDatabaseURL {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"ADDR="+net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		"DATABASE_URL="+databaseURL,
		"LOGTO_ISSUER=http://127.0.0.1:9/oidc",
		"STUN_ADDR=127.0.0.1:0",
	)
	return newLauncherProc(t, cmd)
}

// waitHealthy polls /healthz until the server reports ok, the launcher exits
// or the timeout passes.
func (p *launcherProc) waitHealthy(t *testing.T, port int) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(launcherHealthTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatalf("the launcher exited before the server became healthy (status %d):\n%s", p.cmd.ProcessState.ExitCode(), p.output)
		default:
		}
		if resp, err := client.Get(url); err == nil {
			var body struct {
				Status string `json:"status"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && body.Status == "ok" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the server did not become healthy within %s:\n%s", launcherHealthTimeout, p.output)
}

// adminSnapshot lists the user tables of the administration database and the
// row count of goose_db_version when it exists.
type adminSnapshot struct {
	tables         []string
	migrationsRows int64
}

func takeAdminSnapshot(t *testing.T, admin string) adminSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		// pgx connect errors name the host and user but never the password
		// or the URL.
		t.Fatalf("connect to the administration database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT table_schema || '.' || table_name FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema') ORDER BY 1`)
	if err != nil {
		t.Fatalf("list administration database tables: %v", err)
	}
	var snap adminSnapshot
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		snap.tables = append(snap.tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list administration database tables: %v", err)
	}
	snap.migrationsRows = -1
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("check goose_db_version: %v", err)
	}
	if exists {
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM goose_db_version").Scan(&snap.migrationsRows); err != nil {
			t.Fatalf("count goose_db_version: %v", err)
		}
	}
	return snap
}

// TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase runs the real
// launcher, which builds and runs the real server, against a database that
// the test generated with dbtest. It owns that database, so the
// administration database is not written, and it checks that the service
// becomes healthy on loopback and that SIGTERM drains it with status 143.
// It skips without OWNSAND_TEST_DATABASE_URL, without node and go on the path,
// and on Windows (build constraint).
func TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase(t *testing.T) {
	admin := dbtest.URL(t)
	for _, tool := range []string{"node", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on the path; skipping the launcher test", tool)
		}
	}
	before := takeAdminSnapshot(t, admin)

	// The fixture registers its database drop first, so the launcher cleanup
	// below (registered later) runs before the drop.
	databaseURL := dbtest.NewURL(t)
	port := freeLoopbackPort(t)
	launcher := startLauncher(t, databaseURL, port)
	t.Cleanup(launcher.stop)

	launcher.waitHealthy(t, port)
	if err := launcher.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to the launcher: %v", err)
	}
	select {
	case <-launcher.done:
	case <-time.After(launcherDrainTimeout):
		t.Fatalf("the launcher did not exit within %s after SIGTERM:\n%s", launcherDrainTimeout, launcher.output)
	}
	if code := launcher.cmd.ProcessState.ExitCode(); code != statusSigterm {
		t.Fatalf("launcher exit status = %d, want %d:\n%s", code, statusSigterm, launcher.output)
	}
	if !strings.Contains(launcher.output.String(), "server_shutdown_completed") {
		t.Fatalf("the output has no server_shutdown_completed record:\n%s", launcher.output)
	}

	after := takeAdminSnapshot(t, admin)
	if !slices.Equal(before.tables, after.tables) || before.migrationsRows != after.migrationsRows {
		t.Fatalf("the administration database changed: tables %v -> %v, goose_db_version rows %d -> %d",
			before.tables, after.tables, before.migrationsRows, after.migrationsRows)
	}
}

// processAlive reports whether pid is a live process. A zombie that nobody has
// reaped yet counts as gone: it holds no pipe and cannot run.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// readPIDFile waits for a pid file that a fixture script writes.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fixture script did not write %s", path)
	return 0
}

// TestLauncherTeardownBoundedWhenLeaderExitedAndDescendantHoldsPipe checks
// that stop returns within a bounded budget and kills the descendant when the
// launcher leader has exited and a descendant still holds the inherited
// stdout and stderr pipe. Without cmd.WaitDelay, cmd.Wait blocks until that
// descendant closes the pipe, and without the group kills the descendant
// outlives the test. It needs no database, node or go.
func TestLauncherTeardownBoundedWhenLeaderExitedAndDescendantHoldsPipe(t *testing.T) {
	for _, tool := range []string{"sh", "sleep", "pgrep", "ps"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on the path; skipping the teardown test", tool)
		}
	}
	_, setsidErr := exec.LookPath("setsid")

	tests := []struct {
		name string
		// script starts the descendant, writes its pid to $1 and exits.
		script string
		// needsSetsid marks the cases that detach the descendant into its own
		// session and process group.
		needsSetsid bool
	}{
		{
			// The descendant stays in the leader's process group.
			name:   "same process group, leader exits at once",
			script: `sleep 300 & echo $! > "$1"; exit 0`,
		},
		{
			// The descendant is detached, like the build and the server that
			// the launcher starts. The leader lingers long enough for the
			// tracker to record the descendant, then exits.
			name:        "own process group, leader exits after the tracker saw the descendant",
			script:      `setsid sleep 300 & echo $! > "$1"; sleep 1; exit 0`,
			needsSetsid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsSetsid && setsidErr != nil {
				t.Skipf("setsid is not on the path: %v", setsidErr)
			}
			pidFile := filepath.Join(t.TempDir(), "descendant.pid")
			cmd := exec.Command("sh", "-c", tt.script, "sh", pidFile)
			p := newLauncherProc(t, cmd)
			descendant := readPIDFile(t, pidFile)
			// Never leak the descendant if the assertions below fail.
			t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })

			// Wait until the leader has exited, so the descendant is orphaned
			// from the leader. cmd.ProcessState is not read here: cmd.Wait
			// sets it from another goroutine.
			leader := p.cmd.Process.Pid
			deadline := time.Now().Add(10 * time.Second)
			for processAlive(leader) {
				if time.Now().After(deadline) {
					t.Fatal("the fixture leader did not exit")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !processAlive(descendant) {
				t.Fatal("the descendant exited with its leader; the fixture does not hold the pipe")
			}

			start := time.Now()
			p.stop()
			elapsed := time.Since(start)
			if budget := launcherPipeDrainDelay + 5*time.Second; elapsed > budget {
				t.Errorf("stop took %s, want at most %s", elapsed, budget)
			}
			select {
			case <-p.done:
			default:
				t.Error("stop returned before the launcher was reaped")
			}
			deadline = time.Now().Add(5 * time.Second)
			for processAlive(descendant) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if processAlive(descendant) {
				t.Errorf("the descendant %d is still running after stop", descendant)
			}
		})
	}
}
