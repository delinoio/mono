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
	cmd    *exec.Cmd
	output *lockedBuffer
	done   chan struct{}
}

// childPIDs lists the direct children of pid. The launcher runs the server in
// its own process group, so a SIGKILL of the launcher group does not reach
// it; the test finds the server through its parent instead. It is best effort:
// without pgrep it returns nothing.
func childPIDs(pid int) []int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(field); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// stop ends the launcher and its server and waits until the launcher is
// reaped. It first forwards SIGTERM, which drains the server. If the launcher
// is still running after launcherDrainTimeout, it kills the launcher's
// children, then the launcher's process group, and waits again. It is safe to
// call more than once.
func (p *launcherProc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(launcherDrainTimeout):
	}
	for _, pid := range childPIDs(p.cmd.Process.Pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
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
	// Own process group, so a failure cleanup can kill the launcher group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &lockedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the launcher: %v", err)
	}
	p := &launcherProc{cmd: cmd, output: output, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p
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
