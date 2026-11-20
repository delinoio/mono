package e2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Environment variables of the acceptance test.
const (
	// envE2E turns the test on.
	envE2E = "OWNSAND_E2E"
	// envDatabaseURL names a Postgres server where the test may create and
	// drop a database. The test falls back to OWNSAND_TEST_DATABASE_URL.
	envDatabaseURL = "OWNSAND_E2E_DATABASE_URL"
	// envFCDir names a directory with the pinned `firecracker` binary, a guest
	// `vmlinux` and a static `busybox`. The test builds the root file system
	// from busybox and the current ownsand-guest, so the guest helper always
	// matches the agent under test.
	envFCDir = "OWNSAND_E2E_FC_DIR"
	// envLong adds the steps that take longer than 15 minutes (the relay
	// stream rotation).
	envLong = "OWNSAND_E2E_LONG"
	// envKeepLogs keeps the process logs in this directory after the run.
	envKeepLogs = "OWNSAND_E2E_LOG_DIR"
)

// apiResource is the audience of minted user tokens. It is the control
// plane default, so the test does not need to set OWNSAND_API_RESOURCE.
const apiResource = "https://ownsand.api.delino.io"

// requiredScope is the scope that the control plane requires in user access
// tokens.
const requiredScope = "ownsand:access"

type harness struct {
	t       *testing.T
	root    string
	bins    string
	logDir  string
	fcDir   string
	image   string // directory with vmlinux and rootfs.ext4
	dbURL   string
	dropDB  func()
	issuer  *issuer
	apiAddr string
	stun    string
	origin  string

	mu    sync.Mutex
	procs []*proc
}

type proc struct {
	name string
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv(envE2E) != "1" {
		t.Skipf("set %s=1, %s and %s to run the OwnSand end-to-end acceptance test", envE2E, envDatabaseURL, envFCDir)
	}
	dbBase := os.Getenv(envDatabaseURL)
	if dbBase == "" {
		dbBase = os.Getenv("OWNSAND_TEST_DATABASE_URL")
	}
	fcDir := os.Getenv(envFCDir)
	if dbBase == "" || fcDir == "" {
		t.Fatalf("%s=1 needs %s (or OWNSAND_TEST_DATABASE_URL) and %s", envE2E, envDatabaseURL, envFCDir)
	}
	for _, f := range []string{"firecracker", "vmlinux", "busybox"} {
		if _, err := os.Stat(filepath.Join(fcDir, f)); err != nil {
			t.Fatalf("%s: %v", envFCDir, err)
		}
	}
	h := &harness{t: t, fcDir: fcDir, bins: t.TempDir(), image: t.TempDir()}
	h.logDir = os.Getenv(envKeepLogs)
	if h.logDir == "" {
		h.logDir = t.TempDir()
	} else if err := os.MkdirAll(h.logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	h.root = filepath.Dir(strings.TrimSpace(string(out)))
	h.build()
	h.buildRootfs()
	h.createDatabase(dbBase)
	h.issuer = newIssuer(t)
	t.Cleanup(h.stopAll)
	return h
}

func (h *harness) build() {
	h.t.Helper()
	for name, pkg := range map[string]string{
		"ownsand-api":   "./servers/ownsand-api/cmd/ownsand-api",
		"ownsand-agent": "./cmds/ownsand-agent",
		"ownsand":       "./cmds/ownsand",
	} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(h.bins, name), pkg)
		cmd.Dir = h.root
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}
}

// guestInit is the init of the test root file system: it mounts the kernel
// file systems and starts the guest helper.
const guestInit = `#!/bin/busybox sh
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null
/bin/busybox mount -t tmpfs tmpfs /tmp
/usr/local/bin/ownsand-guest &
while true; do wait; /bin/busybox sleep 1; done
`

// buildRootfs builds rootfs.ext4 with busybox and a static ownsand-guest.
// mkfs.ext4 -d fills the file system without root privileges.
func (h *harness) buildRootfs() {
	h.t.Helper()
	src := h.t.TempDir()
	for _, d := range []string{"bin", "sbin", "proc", "sys", "dev", "tmp", "root", "etc", "var/lib", "usr/local/bin"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	bb, err := os.ReadFile(filepath.Join(h.fcDir, "busybox"))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "busybox"), bb, 0o755); err != nil {
		h.t.Fatal(err)
	}
	for _, applet := range []string{"sh", "cat", "echo", "sleep", "ls", "mount"} {
		if err := os.Symlink("busybox", filepath.Join(src, "bin", applet)); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "sbin", "init"), []byte(guestInit), 0o755); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(src, "usr", "local", "bin", "ownsand-guest"), "./cmds/ownsand-guest")
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("build ownsand-guest: %v\n%s", err, out)
	}
	mkfs := "mkfs.ext4"
	if p, err := exec.LookPath(mkfs); err == nil {
		mkfs = p
	} else {
		for _, p := range []string{"/usr/sbin/mkfs.ext4", "/sbin/mkfs.ext4"} {
			if _, err := os.Stat(p); err == nil {
				mkfs = p
			}
		}
	}
	rootfs := filepath.Join(h.image, "rootfs.ext4")
	if err := os.WriteFile(rootfs, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Truncate(rootfs, 256<<20); err != nil {
		h.t.Fatal(err)
	}
	if out, err := exec.Command(mkfs, "-q", "-F", "-d", src, rootfs).CombinedOutput(); err != nil {
		h.t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	if err := os.Symlink(filepath.Join(h.fcDir, "vmlinux"), filepath.Join(h.image, "vmlinux")); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) createDatabase(base string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		h.t.Fatalf("connect to postgres: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("ownsand_e2e_%x", b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = conn.Close(ctx)
		h.t.Fatalf("create database: %v", err)
	}
	_ = conn.Close(ctx)
	u, err := url.Parse(base)
	if err != nil {
		h.t.Fatal(err)
	}
	u.Path = "/" + name
	h.dbURL = u.String()
	h.dropDB = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	}
}

func freeTCP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func freeUDP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}

// start runs a binary with its output in a log file of the log directory.
func (h *harness) start(name string, env []string, args ...string) *proc {
	h.t.Helper()
	f, err := os.OpenFile(filepath.Join(h.logDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(h.bins, binaryOf(name)), args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = f
	cmd.Stderr = f
	// Its own process group, so a stop also ends the Firecracker children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	p := &proc{name: name, cmd: cmd, log: f, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	h.mu.Lock()
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	return p
}

func binaryOf(name string) string {
	switch {
	case strings.HasPrefix(name, "agent"):
		return "ownsand-agent"
	case strings.HasPrefix(name, "api"):
		return "ownsand-api"
	}
	return name
}

// stop sends SIGTERM to the process group and SIGKILL after the timeout.
func (p *proc) stop(timeout time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
	_ = p.log.Close()
}

// kill ends the process group at once, like a crash.
func (p *proc) kill() {
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
	_ = p.log.Close()
}

func (h *harness) stopAll() {
	h.mu.Lock()
	procs := append([]*proc(nil), h.procs...)
	h.mu.Unlock()
	for i := len(procs) - 1; i >= 0; i-- {
		procs[i].stop(15 * time.Second)
	}
	if h.t.Failed() {
		for _, p := range procs {
			h.t.Logf("--- tail of %s.log ---\n%s", p.name, tail(filepath.Join(h.logDir, p.name+".log"), 60))
		}
	}
	if h.dropDB != nil {
		h.dropDB()
	}
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// startAPI starts the control plane on free loopback ports.
func (h *harness) startAPI() {
	h.t.Helper()
	if h.apiAddr == "" {
		h.apiAddr = freeTCP(h.t)
		h.stun = freeUDP(h.t)
		h.origin = "http://" + h.apiAddr
	}
	var key [32]byte
	_, _ = rand.Read(key[:])
	h.start("api", []string{
		"ADDR=" + h.apiAddr,
		"LOG_LEVEL=debug",
		"DATABASE_URL=" + h.dbURL,
		"LOGTO_ENDPOINT=" + h.issuer.endpoint,
		"OWNSAND_NONCE_KEY=" + base64.StdEncoding.EncodeToString(key[:]),
		"STUN_ADDR=" + h.stun,
		"OWNSAND_STUN_URLS=stun:" + h.stun,
	})
	h.waitHTTP(h.origin+"/healthz", 60*time.Second)
}

func (h *harness) waitHTTP(u string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("%s did not answer within %v", u, timeout)
}

// agentStateDir prepares a state directory with the base image.
func (h *harness) agentStateDir() string {
	h.t.Helper()
	// A short path: Firecracker's vsock socket lives in the state directory
	// and a Unix socket path has at most 108 bytes.
	dir, err := os.MkdirTemp("", "osa-")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = os.RemoveAll(dir) })
	img := filepath.Join(dir, "images", "linux")
	if err := os.MkdirAll(img, 0o700); err != nil {
		h.t.Fatal(err)
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4"} {
		if err := os.Symlink(filepath.Join(h.image, f), filepath.Join(img, f)); err != nil {
			h.t.Fatal(err)
		}
	}
	return dir
}

func (h *harness) agentEnv() []string {
	return []string{
		"OWNSAND_AGENT_FIRECRACKER=" + filepath.Join(h.fcDir, "firecracker"),
		"OWNSAND_AGENT_ALLOW_UNSIGNED_IMAGE=1",
		"NO_COLOR=1",
	}
}

// runCLI runs the ownsand CLI and returns stdout, stderr and the exit code.
func (h *harness) runCLI(env []string, stdin []byte, args ...string) (string, string, int) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.bins, "ownsand"), args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// issuer is a minimal Logto stand-in: it serves a JWKS at <endpoint>/oidc/jwks
// and mints access tokens for the OwnSand API resource.
type issuer struct {
	endpoint string
	key      *rsa.PrivateKey
	kid      string
}

func newIssuer(t *testing.T) *issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key, kid: "e2e-key"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oidc/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": is.kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	is.endpoint = srv.URL
	return is
}

// token mints an end-user access token for sub.
func (is *issuer) token(sub string) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "at+jwt", "kid": is.kid})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iss": is.endpoint + "/oidc", "aud": apiResource, "sub": sub, "client_id": "e2e-client",
		"scope": "openid " + requiredScope, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signed := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
