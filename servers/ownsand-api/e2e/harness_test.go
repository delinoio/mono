// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsand "github.com/delinoio/mono/go/ownsand"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

// EnvE2E turns the end-to-end suite on. The suite also needs
// OWNSAND_TEST_DATABASE_URL (see dbtest).
const EnvE2E = "OWNSAND_E2E"

const (
	testIssuer   = "http://e2e-issuer.invalid/oidc"
	testAudience = "https://ownsand.api.delino.io"
	testKeyID    = "e2e-key"
)

// stack is one running control plane with one enrolled agent.
type stack struct {
	t        *testing.T
	bins     string
	apiURL   string
	stunAddr string
	key      *ecdsa.PrivateKey
	userSub  string
	orgID    string
	// deviceID is the device of the first agent.
	deviceID string
	agents   []agentProc
	apiKey   string
	logs     *logSink
	objects  *fakeS3
	// apiEnv and api are the environment and process of the control plane,
	// so a test can kill it and start it again over the same database. The
	// name of api prefixes its lines in logs.
	apiEnv []string
	api    *proc
	// work is the directory of the agent state directories.
	work string
	// processRuntime is true when the agents use the process runtime, whose
	// snapshots hold only the files of the sandbox root.
	processRuntime bool
	// accounts is the fake delino-accounts API of usage reporting and
	// billing, and billingAccountID the billing account that the
	// organization is linked to, in which the user is the billing owner.
	accounts         *accountstest.Server
	billingAccountID string
}

// requireE2E skips unless the suite is enabled and a database is set.
func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvE2E) != "1" {
		t.Skipf("%s is not 1; skipping the OwnSand end-to-end suite", EnvE2E)
	}
	dbtest.URL(t)
	if testing.Short() {
		t.Skip("skipping the end-to-end suite in -short mode")
	}
}

// linuxGuestRuntime is the agent runtime that runs the LINUX sandboxes of a
// stack that does not select its own runtime.
type linuxGuestRuntime int

const (
	// linuxGuestRuntimeProcess is the process runtime, on a Linux host.
	linuxGuestRuntimeProcess linuxGuestRuntime = iota
	// linuxGuestRuntimeVirtualization is the Virtualization.framework
	// runtime with the base image of TestLifecycleVirtualization, on a
	// macOS host.
	linuxGuestRuntimeVirtualization
	// linuxGuestRuntimeUnavailable means that the host cannot run the
	// LINUX sandboxes of the suite, so the test is skipped.
	linuxGuestRuntimeUnavailable
)

// defaultLinuxGuestRuntime selects the runtime of the LINUX sandboxes for a
// host of goos, and returns the skip reason when there is none. The process
// runtime runs the guest helper as a host process, so it runs only guests of
// the host OS (MACOS on a macOS host) and runs the suite's LINUX sandboxes
// only on Linux. On a macOS host, the Virtualization.framework runtime runs
// them when vzKernel and vzRootfs (OWNSAND_VZ_TEST_KERNEL and
// OWNSAND_VZ_TEST_ROOTFS) name the base image.
func defaultLinuxGuestRuntime(goos, vzKernel, vzRootfs string) (linuxGuestRuntime, string) {
	switch {
	case goos == "linux":
		return linuxGuestRuntimeProcess, ""
	case goos == "darwin" && vzKernel != "" && vzRootfs != "":
		return linuxGuestRuntimeVirtualization, ""
	case goos == "darwin":
		return linuxGuestRuntimeUnavailable, "the process runtime runs only guests of the host OS and the suite creates LINUX sandboxes; set " +
			envVZKernel + " and " + envVZRootfs + " to run them with the Virtualization.framework runtime on this macOS host"
	default:
		return linuxGuestRuntimeUnavailable, "the process runtime runs only guests of the host OS and the suite creates LINUX sandboxes; run it on a Linux or macOS host (" + goos + " host)"
	}
}

// noSandboxProcessRuntimeSkip returns the skip reason of a stack that creates
// no sandbox and selects no runtime, or "" when the host can run it. Such a
// stack still starts its agent with the process runtime, which exists only on
// Linux and macOS hosts: on a Windows host the agent fails at startup, after
// the binaries are built and the test database is created.
func noSandboxProcessRuntimeSkip(goos string) string {
	if goos == "windows" {
		return "the agent process runtime needs a Linux or macOS host, even for a test that creates no sandbox (" + goos + " host)"
	}
	return ""
}

// findModuleRoot returns the directory of the root go.mod. When go env fails,
// the error includes the stderr of the command, so the cause is not lost.
func findModuleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return "", fmt.Errorf("go env GOMOD: %w: %s", err, stderr)
			}
		}
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

// moduleRoot returns the directory of the root go.mod and fails the test when
// it cannot be resolved.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// buildSteps are the steps of one binary build. Tests replace them to check
// the failure handling without running the Go toolchain.
type buildSteps struct {
	// mkdirTemp creates the output directory.
	mkdirTemp func() (string, error)
	// moduleRoot resolves the directory of the root go.mod.
	moduleRoot func() (string, error)
	// build builds the binaries into dir from the module root.
	build func(root, dir string) error
}

// goBuildSteps are the steps that build the real binaries.
var goBuildSteps = buildSteps{
	mkdirTemp:  func() (string, error) { return os.MkdirTemp("", "ownsand-e2e-bin-") },
	moduleRoot: findModuleRoot,
	build: func(root, dir string) error {
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", dir+string(filepath.Separator),
			"./servers/ownsand-api/cmd/ownsand-api", "./cmds/ownsand-agent", "./cmds/ownsand-guest", "./cmds/ownsand")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}
		return nil
	},
}

// newBinaryBuilder returns a function that runs the steps once and returns the
// same directory or the same error on every call. A step reports every failure
// as an error and never ends the calling goroutine: sync.Once treats a
// runtime.Goexit, such as t.Fatal, as a normal return, so later callers would
// get an unbuilt directory and no error. A failed build removes its directory.
func newBinaryBuilder(steps buildSteps) func() (string, error) {
	return sync.OnceValues(func() (string, error) {
		dir, err := steps.mkdirTemp()
		if err != nil {
			return "", err
		}
		root, err := steps.moduleRoot()
		if err == nil {
			err = steps.build(root, dir)
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	})
}

var (
	// buildDir is the directory of the built binaries, which TestMain removes.
	// It stays empty until a build succeeded.
	buildDir     string
	buildDirOnce sync.Once
	buildCached  = newBinaryBuilder(goBuildSteps)
)

// buildBinaries builds the control plane, agent, guest helper and CLI once
// per test binary run. Every call fails the test when the build failed.
func buildBinaries(t *testing.T) string {
	t.Helper()
	dir, err := buildCached()
	if err != nil {
		t.Fatal(err)
	}
	buildDirOnce.Do(func() { buildDir = dir })
	return dir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func freeUDPPort(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}

// logSink keeps the output of the child processes and prints it when the
// test fails. Lines are prefixed with the process name and ": ".
type logSink struct {
	mu    sync.Mutex
	lines []string
	// scanners counts the scanner goroutines that still run, so a test can
	// check that a finished process released its log resources.
	scanners atomic.Int32
}

// writer returns a writer for one output stream of the process name. A
// goroutine stores the lines written to it until the writer is closed; the
// returned channel closes after that goroutine stored the last line and
// exited. exec.Cmd does not close a custom writer, so the owner must.
func (l *logSink) writer(name string) (io.WriteCloser, <-chan struct{}) {
	r, w := io.Pipe()
	done := make(chan struct{})
	l.scanners.Add(1)
	go func() {
		defer close(done)
		defer l.scanners.Add(-1)
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64<<10), 1<<20)
		for s.Scan() {
			l.mu.Lock()
			l.lines = append(l.lines, name+": "+s.Text())
			l.mu.Unlock()
		}
		// Closing the reader fails later writes of the process instead of
		// blocking it when the scan stopped on an error, such as a line that
		// is too long.
		_ = r.CloseWithError(s.Err())
	}()
	return w, done
}

func (l *logSink) dump(t *testing.T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if os.Getenv("OWNSAND_E2E_LOGS") == "" && !t.Failed() {
		return
	}
	for _, line := range l.lines {
		t.Log(line)
	}
}

// hasLine reports whether the sink stored the exact prefixed line.
func (l *logSink) hasLine(line string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.lines, line)
}

// hasLineContaining reports whether the process name logged a line that
// contains text.
func (l *logSink) hasLineContaining(name, text string) bool {
	prefix := name + ": "
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if rest, ok := strings.CutPrefix(line, prefix); ok && strings.Contains(rest, text) {
			return true
		}
	}
	return false
}

// hasStateReport reports whether the process name logged an
// agent_state_report event for the device. The control plane logs this event
// after it marked the connection of the device as reconciled, which is when
// placement can use the device.
func (l *logSink) hasStateReport(name, deviceID string) bool {
	prefix := name + ": "
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		text, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var event struct {
			Msg      string `json:"msg"`
			DeviceID string `json:"device_id"`
		}
		// Lines that are not JSON log records are not events.
		if json.Unmarshal([]byte(text), &event) != nil {
			continue
		}
		if event.Msg == "agent_state_report" && event.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// proc is a child process that startProcess started.
type proc struct {
	name string
	cmd  *exec.Cmd
	// done closes after the process exited and its log writers are closed
	// and drained.
	done chan struct{}
}

// killProcess kills a child process that startProcess started and waits for
// it to exit, as a crash does: the process gets no chance to shut down.
func killProcess(t *testing.T, p *proc) {
	t.Helper()
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the killed process %s did not exit", p.name)
	}
}

// launchProcess starts a child process and logs its output to logs. When the
// process exits, it closes both log writers and waits for their scanners
// before it closes the done channel of the process.
func launchProcess(logs *logSink, name string, env []string, bin string, args ...string) (*proc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	stdout, stdoutDone := logs.writer(name)
	stderr, stderrDone := logs.writer(name)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	release := func() {
		_ = stdout.Close()
		_ = stderr.Close()
		<-stdoutDone
		<-stderrDone
	}
	if err := cmd.Start(); err != nil {
		release()
		return nil, err
	}
	p := &proc{name: name, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		release()
		close(p.done)
	}()
	return p, nil
}

// startProcess starts a child process that is stopped with SIGTERM when the
// test ends.
func startProcess(t *testing.T, logs *logSink, name string, env []string, bin string, args ...string) *proc {
	t.Helper()
	p, err := launchProcess(logs, name, env, bin, args...)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// stackOptions configures newStack.
type stackOptions struct {
	// agents is the number of enrolled devices; zero means one.
	agents int
	// objects runs the fake S3 store and configures snapshot export.
	objects bool
	// agentEnv selects the agent runtime; nil means the runtime that
	// defaultLinuxGuestRuntime selects for the host.
	agentEnv []string
	// noSandboxes marks a test that creates no sandbox, so it runs with the
	// process runtime on Linux and macOS hosts and is skipped on other
	// hosts (see noSandboxProcessRuntimeSkip).
	noSandboxes bool
}

// agentProc is one enrolled agent process.
type agentProc struct {
	deviceID string
	stateDir string
	// env and proc are the environment and process of the agent, so a test
	// can kill it and start it again over the same state directory.
	env  []string
	proc *proc
}

// newStack starts the control plane, creates an organization with a user
// token, enrolls and starts the agents with the runtime of the options (by
// default the process runtime on Linux and the Virtualization.framework
// runtime on macOS, see defaultLinuxGuestRuntime), sets device limits and
// creates an API key.
func newStack(t *testing.T, opts ...stackOptions) *stack {
	t.Helper()
	requireE2E(t)
	var o stackOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.agents == 0 {
		o.agents = 1
	}
	runtimeEnv := o.agentEnv
	if runtimeEnv == nil && o.noSandboxes {
		// Skip before buildBinaries and the test database, which the agent
		// startup failure on a Windows host would otherwise waste.
		if reason := noSandboxProcessRuntimeSkip(runtime.GOOS); reason != "" {
			t.Skip(reason)
		}
	}
	if runtimeEnv == nil && !o.noSandboxes {
		kernel, rootfs := os.Getenv(envVZKernel), os.Getenv(envVZRootfs)
		switch kind, reason := defaultLinuxGuestRuntime(runtime.GOOS, kernel, rootfs); kind {
		case linuxGuestRuntimeVirtualization:
			runtimeEnv = virtualizationAgentEnv(t, kernel, rootfs)
		case linuxGuestRuntimeUnavailable:
			t.Skip(reason)
		}
	}
	bins := buildBinaries(t)
	logs := &logSink{}
	t.Cleanup(func() { logs.dump(t) })
	work := t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: testKeyID, Algorithm: string(jose.ES256), Use: "sig"}}}
	jwksJSON, err := json.Marshal(jwks)
	if err != nil {
		t.Fatal(err)
	}
	jwksFile := filepath.Join(work, "jwks.json")
	if err := os.WriteFile(jwksFile, jwksJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	s := &stack{t: t, bins: bins, key: key, userSub: "e2e-user-" + randomSuffix(t), logs: logs}
	apiEnv := []string{
		// The e2e clients connect to the server directly, so the TCP peer is
		// the client address for rate limits.
		"ALLOW_DIRECT_CLIENT_ADDRESS=true",
		"LOG_LEVEL=debug",
		"DATABASE_URL=" + dbtest.NewURL(t),
		"LOGTO_ISSUER=" + testIssuer,
		"LOGTO_JWKS_FILE=" + jwksFile,
	}
	if o.objects {
		s.objects = newFakeS3(t)
		apiEnv = append(apiEnv,
			"R2_ENDPOINT="+s.objects.url,
			"R2_BUCKET="+fakeBucket,
			"R2_ACCESS_KEY_ID=e2e-access-key",
			"R2_SECRET_ACCESS_KEY=e2e-secret-key",
			// The minimum part size, so a snapshot of a few MiB has
			// several parts.
			"SNAPSHOT_PART_SIZE_BYTES="+strconv.Itoa(e2ePartSize),
		)
	}
	// Billing: the control plane reports usage to, polls statuses from and
	// links organizations through a fake delino-accounts. The organization
	// below is linked to a billing account in good standing, so the suite
	// runs as an organization in good standing.
	s.accounts = accountstest.New(t, nil)
	apiEnv = append(apiEnv, s.accounts.Env()...)
	s.work = work
	s.startAPI(apiEnv)

	ctx := s.ctx(2 * time.Minute)
	user := s.userClient(ownsand.TransportAuto)
	org, err := user.Organizations().Create(ctx, &ownsandv1.CreateOrganizationRequest{Name: "e2e"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	s.orgID = org.GetOrganization().GetOrganizationId()
	s.billingAccountID = e2eBillingAccountID
	s.accounts.AddAccount(s.billingAccountID, "e2e", map[string]accountsv1.BillingAccountRole{s.userSub: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	if b := s.linkBilling(ctx); b.GetPhase() != ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED {
		t.Fatalf("billing phase after the link = %v", b.GetPhase())
	}
	user = s.userClient(ownsand.TransportAuto)

	if runtimeEnv == nil {
		runtimeEnv = []string{"OWNSAND_AGENT_RUNTIME=process", "OWNSAND_AGENT_GUEST_BINARY=" + filepath.Join(bins, "ownsand-guest")}
		s.processRuntime = true
	}
	for i := range o.agents {
		s.agents = append(s.agents, s.startAgent(ctx, user, work, i, runtimeEnv))
	}
	s.deviceID = s.agents[0].deviceID
	k, err := user.APIKeys().Create(ctx, &ownsandv1.CreateApiKeyRequest{OrganizationId: s.orgID, Name: "e2e"})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	s.apiKey = k.GetApiKeySecret()
	return s
}

// e2ePartSize is the export part size of the suite: the minimum, so a
// snapshot of a few MiB has several parts.
const e2ePartSize = 5 << 20

// e2eBillingAccountID is the billing account of the suite's organization.
const e2eBillingAccountID = "01900000-0000-7000-8000-0000000e2e01"

// bearerToken adds a bearer credential to each unary request.
type bearerToken func() string

func (b bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b())
		return next(ctx, req)
	}
}

func (b bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (b bearerToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// billingClient returns a BillingService client of the user. The Go SDK has
// no billing client in this layer, so the suite calls the service
// directly.
func (s *stack) billingClient() ownsandv1connect.BillingServiceClient {
	return ownsandv1connect.NewBillingServiceClient(http.DefaultClient, s.apiURL, connect.WithInterceptors(bearerToken(s.userToken)))
}

// linkBilling links the organization to the suite's billing account (again)
// and returns the billing status from the response; a repeated link applies
// the current status that the fake delino-accounts returns.
func (s *stack) linkBilling(ctx context.Context) *ownsandv1.OrganizationBilling {
	s.t.Helper()
	resp, err := s.billingClient().LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: s.orgID, BillingAccountId: s.billingAccountID}))
	if err != nil {
		s.t.Fatalf("LinkBillingAccount: %v", err)
	}
	return resp.Msg.GetBilling()
}

// startAgent enrolls agent i with a new enrollment token, runs it, waits
// until its device is online and sets its limits.
func (s *stack) startAgent(ctx context.Context, user *ownsand.Client, work string, i int, runtimeEnv []string) agentProc {
	t := s.t
	t.Helper()
	tok, err := user.Devices().CreateEnrollmentToken(ctx, &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: s.orgID, Ttl: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatalf("CreateEnrollmentToken: %v", err)
	}
	stateDir := filepath.Join(work, fmt.Sprintf("agent-%d", i))
	agentEnv := append([]string{
		"OWNSAND_AGENT_STATE_DIR=" + stateDir,
		"OWNSAND_AGENT_LOG_LEVEL=debug",
		"NO_COLOR=1",
	}, runtimeEnv...)
	enroll := exec.CommandContext(ctx, filepath.Join(s.bins, "ownsand-agent"), "enroll", "--api-url", s.apiURL, "--name", fmt.Sprintf("e2e-device-%d", i))
	enroll.Env = append(os.Environ(), agentEnv...)
	enroll.Stdin = strings.NewReader(tok.GetEnrollmentToken() + "\n")
	if out, err := enroll.CombinedOutput(); err != nil {
		t.Fatalf("ownsand-agent enroll: %v\n%s", err, out)
	}
	agent := startProcess(t, s.logs, fmt.Sprintf("agent%d", i), agentEnv, filepath.Join(s.bins, "ownsand-agent"), "run")

	known := map[string]bool{}
	for _, a := range s.agents {
		known[a.deviceID] = true
	}
	deviceID := s.waitDeviceOnline(user, known)
	if _, err := user.Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: s.orgID, DeviceId: deviceID,
		Limits: &ownsandv1.DeviceLimitsInput{VcpuCount: proto.Int32(8), MemoryMib: proto.Int64(16384), DiskGib: proto.Int64(100), VcpuOvercommitPercent: proto.Int32(100)}}); err != nil {
		t.Fatalf("ReplaceDeviceLimits: %v", err)
	}
	return agentProc{deviceID: deviceID, stateDir: stateDir, env: agentEnv, proc: agent}
}

// killAgent kills agent i, as a crash does.
func (s *stack) killAgent(i int) {
	s.t.Helper()
	killProcess(s.t, s.agents[i].proc)
}

// restartAgent starts agent i again over its state directory.
func (s *stack) restartAgent(i int) {
	s.t.Helper()
	a := &s.agents[i]
	a.proc = startProcess(s.t, s.logs, fmt.Sprintf("agent%d-restarted", i), a.env, filepath.Join(s.bins, "ownsand-agent"), "run")
}

// killAPI kills the control plane, as a crash does.
func (s *stack) killAPI() {
	s.t.Helper()
	killProcess(s.t, s.api)
}

// apiStartAttempts bounds the starts of the control plane on new ports.
const apiStartAttempts = 3

// startAPI starts the control plane on free ports with the environment env
// and waits until it is healthy.
//
// freeTCPPort and freeUDPPort close their listeners again, so another process
// (for example a test server of another package that go test runs at the
// same time) can take a port before the control plane binds it. The control
// plane then exits and logs server_exited with the structured
// startup_failure_reason=address_in_use (whatever the native error text is),
// and the other server can answer the health probe. startAPI then starts the
// control plane again on new ports, up to apiStartAttempts times (see
// shouldRetryAPIStart). Any other failure fails the test at once.
func (s *stack) startAPI(env []string) {
	s.t.Helper()
	for attempt := 1; ; attempt++ {
		addr := freeTCPPort(s.t)
		stunAddr := freeUDPPort(s.t)
		s.apiURL = "http://" + addr
		s.stunAddr = stunAddr
		s.apiEnv = append([]string{
			"ADDR=" + addr,
			"STUN_ADDR=" + stunAddr,
			"STUN_URLS=stun:" + stunAddr,
			// No e2e test reads /metrics, and the process inherits the
			// environment of the test run: a METRICS_ADDR of a running
			// local dev server would make the control plane fail to bind
			// it on every attempt. An empty value turns the listener off
			// (exec keeps the last value of a duplicate key). A fixture
			// that needs metrics can set its own METRICS_ADDR in env.
			"METRICS_ADDR=",
		}, env...)
		name := "api"
		if attempt > 1 {
			name = fmt.Sprintf("api-attempt%d", attempt)
		}
		s.api = startProcess(s.t, s.logs, name, s.apiEnv, filepath.Join(s.bins, "ownsand-api"))
		// A 200 counts only after this process logged server_started for the
		// chosen address, so another server that took the port cannot stand in
		// for a control plane that is still loading its configuration or that
		// exits because it could not bind. The wait ends with errProcessExited
		// when the process exits (it logs server_exited first) and is bounded
		// by healthStartupBudget.
		owned := func() bool { return s.logs.hasServerStarted(name, addr) }
		err := pollReadyWhile(s.apiURL+"/healthz", healthStartupBudget, s.api.done, owned)
		if err == nil {
			return
		}
		if !shouldRetryAPIStart(err, attempt, s.logs, name) {
			s.t.Fatalf("the control plane did not become healthy: %v", err)
		}
		s.t.Logf("the control plane could not bind its ports (attempt %d); starting it on new ports", attempt)
	}
}

// restartAPI starts the control plane again on the same address over the
// same database and waits until the replacement process itself is healthy.
//
// The wait stops at once when the replacement exits, and an answer on the
// address counts only after the replacement logged "server_started", which
// it does after it bound the listener. Another server that holds the address
// therefore cannot stand in for the replacement.
func (s *stack) restartAPI() {
	s.t.Helper()
	const name = "api-restarted"
	s.api = startProcess(s.t, s.logs, name, s.apiEnv, filepath.Join(s.bins, "ownsand-api"))
	owned := func() bool { return s.logs.hasLogMessage(name, "server_started") }
	if err := pollReadyWhile(s.apiURL+"/healthz", healthStartupBudget, s.api.done, owned); err != nil {
		s.t.Fatalf("the restarted control plane did not become healthy: %v", err)
	}
}

func randomSuffix(t *testing.T) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

func (s *stack) ctx(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	s.t.Cleanup(cancel)
	return ctx
}

// healthStartupBudget bounds the whole wait for the control plane: every
// request and every retry delay stops when it expires.
const healthStartupBudget = 60 * time.Second

// healthPollInterval is the delay between health probes.
const healthPollInterval = 200 * time.Millisecond

// pollHealthy probes url until it answers 200. The budget covers the HTTP
// requests too, so a connection that is accepted but never answered cannot
// outlive it.
func pollHealthy(url string, budget time.Duration) error {
	return pollHealthyWhile(url, budget, nil)
}

// errProcessExited reports that the probed process exited before it became
// healthy.
var errProcessExited = errors.New("the process exited")

// pollHealthyWhile is pollHealthy that also stops with errProcessExited when
// exited closes, so a server that exited is not waited for. It returns nil at
// the first 200 answer unless exited is closed at that moment, in which case
// it returns errProcessExited even though the probe answered 200. A nil
// exited never closes.
//
// It does not show that the process behind exited gave the answer: while that
// process still runs, a 200 from another server on the same address counts.
// A caller that must know that the process owns the listener uses
// pollReadyWhile with an ownership check, as startAPI and restartAPI do.
func pollHealthyWhile(url string, budget time.Duration, exited <-chan struct{}) error {
	return pollReadyWhile(url, budget, exited, nil)
}

// errNotOwned describes a healthy answer that the probed process cannot be
// shown to have given yet.
var errNotOwned = errors.New("answered 200 before the process reported that it owns the listener")

// pollReadyWhile is pollHealthyWhile with a process-ownership check: when
// owned is not nil, a 200 counts only after owned reports true. owned is how
// the caller shows that the probed process itself bound the listener, for
// example by its "server_started" log record, so a 200 from another server
// that holds the address does not count even before the probed process has
// exited.
//
// Process exit cancels any active probe. The poller checks for exit before
// starting a probe, after it returns, and before accepting readiness or
// reporting a timeout. A timeout keeps the last useful probe result.
func pollReadyWhile(url string, budget time.Duration, exited <-chan struct{}, owned func() bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if exited != nil {
		go func() {
			select {
			case <-exited:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	var lastErr error
	processExit := func() error {
		select {
		case <-exited:
			if lastErr == nil {
				return fmt.Errorf("%w before it became healthy", errProcessExited)
			}
			return fmt.Errorf("%w before it became healthy (last probe: %v)", errProcessExited, lastErr)
		default:
			return nil
		}
	}
	for {
		if err := processExit(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = errNotOwned
			} else {
				lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			}
		} else if ctx.Err() == nil || lastErr == nil {
			// Cancellation provides no new probe result. Keep any earlier result.
			lastErr = err
		}
		if err := processExit(); err != nil {
			return err
		}
		if err == nil && resp.StatusCode == http.StatusOK && (owned == nil || owned()) {
			return processExit()
		}
		select {
		case <-ctx.Done():
			if err := processExit(); err != nil {
				return err
			}
			return fmt.Errorf("not healthy within %s: %w", budget, lastErr)
		case <-time.After(healthPollInterval):
		}
	}
}

// userToken signs an RFC 9068 access token for the test issuer.
func (s *stack) userToken() string {
	s.t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: s.key},
		(&jose.SignerOptions{}).WithType("at+jwt").WithHeader("kid", testKeyID))
	if err != nil {
		s.t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss":       testIssuer,
		"aud":       testAudience,
		"sub":       s.userSub,
		"client_id": "e2e-console",
		"iat":       now.Unix(),
		"exp":       now.Add(time.Hour).Unix(),
		"scope":     "openid ownsand:use",
	}
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		s.t.Fatal(err)
	}
	return tok
}

func (s *stack) userClient(transport ownsand.Transport) *ownsand.Client {
	s.t.Helper()
	tok := s.userToken()
	c, err := ownsand.New(ownsand.Options{
		BaseURL:        s.apiURL,
		TokenSource:    func(context.Context) (string, error) { return tok, nil },
		OrganizationID: s.orgID,
		Transport:      transport,
		LookupEnv:      func(string) (string, bool) { return "", false },
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

func (s *stack) apiKeyClient(transport ownsand.Transport) *ownsand.Client {
	s.t.Helper()
	c, err := ownsand.New(ownsand.Options{
		BaseURL:        s.apiURL,
		APIKey:         s.apiKey,
		OrganizationID: s.orgID,
		Transport:      transport,
		LookupEnv:      func(string) (string, bool) { return "", false },
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// deviceWaitTimeout bounds how long a started agent may take to come online
// and to be reconciled by the control plane.
const deviceWaitTimeout = time.Minute

// waitDeviceOnline returns the ID of a device that is not in skip, is ONLINE
// and has been reconciled by the current control plane process. ONLINE is
// stored when the control plane handles the first state report, but placement
// only uses the device after the connection is marked as reconciled, which
// the control plane logs as agent_state_report afterwards. A sandbox created
// right after this returns must not race with that.
func (s *stack) waitDeviceOnline(c *ownsand.Client, skip map[string]bool) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deviceWaitTimeout)
	defer cancel()
	apiName := s.api.name
	id, err := awaitDeviceReconciled(ctx, 200*time.Millisecond, skip,
		func(ctx context.Context) ([]string, error) {
			res, err := c.Devices().List(ctx, &ownsandv1.ListDevicesRequest{OrganizationId: s.orgID})
			if err != nil {
				return nil, err
			}
			var online []string
			for _, d := range res.GetDevices() {
				if d.GetState() == ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
					online = append(online, d.GetDeviceId())
				}
			}
			return online, nil
		},
		func(id string) bool { return s.logs.hasStateReport(apiName, id) },
		apiName)
	if err != nil {
		s.t.Fatal(err)
	}
	return id
}

// awaitDeviceReconciled polls until a device that is not in skip is listed as
// online and reconciled(id) reports true, and returns its ID. When ctx ends
// first, the error names the condition that was missing. process names the
// control plane process for that message.
func awaitDeviceReconciled(ctx context.Context, interval time.Duration, skip map[string]bool,
	listOnline func(context.Context) ([]string, error), reconciled func(id string) bool, process string) (string, error) {
	// pending is the last online device that was not reconciled.
	pending := ""
	for {
		online, err := listOnline(ctx)
		switch {
		case err == nil:
			pending = ""
			for _, id := range online {
				if skip[id] {
					continue
				}
				if reconciled(id) {
					return id, nil
				}
				if pending == "" {
					pending = id
				}
			}
		case ctx.Err() == nil:
			return "", fmt.Errorf("ListDevices: %w", err)
		}
		select {
		case <-ctx.Done():
			if pending == "" {
				return "", errors.New("the device did not come online: no new device is ONLINE")
			}
			return "", fmt.Errorf("device %s is ONLINE but agent_state_report was not observed for it in the logs of process %s", pending, process)
		case <-time.After(interval):
		}
	}
}

// cliResult is the result of one CLI run.
type cliResult struct {
	code   int
	stdout []byte
	stderr string
}

// cli runs the ownsand CLI with the API key and the organization.
func (s *stack) cli(transport ownsand.Transport, stdin io.Reader, args ...string) cliResult {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(s.bins, "ownsand"), args...)
	cmd.Env = append(os.Environ(),
		"OWNSAND_API_URL="+s.apiURL,
		"OWNSAND_API_KEY="+s.apiKey,
		"OWNSAND_ORG="+s.orgID,
		"OWNSAND_TRANSPORT="+transport.String(),
		"OWNSAND_CONFIG_DIR="+s.t.TempDir(),
		"NO_COLOR=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			s.t.Fatalf("ownsand %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return cliResult{code: code, stdout: stdout.Bytes(), stderr: stderr.String()}
}
