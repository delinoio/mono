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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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

// moduleRoot returns the directory of the root go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// buildBinaries builds the control plane, agent, guest helper and CLI once
// per test binary run.
var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func buildBinaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ownsand-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", dir+string(filepath.Separator),
			"./servers/ownsand-api/cmd/ownsand-api", "./cmds/ownsand-agent", "./cmds/ownsand-guest", "./cmds/ownsand")
		cmd.Dir = moduleRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
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
// test fails. Lines are prefixed with the process name.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) writer(name string) io.Writer {
	r, w := io.Pipe()
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64<<10), 1<<20)
		for s.Scan() {
			l.mu.Lock()
			l.lines = append(l.lines, name+": "+s.Text())
			l.mu.Unlock()
		}
	}()
	return w
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

// startProcess starts a child process that is stopped with SIGTERM when the
// test ends.
func startProcess(t *testing.T, logs *logSink, name string, env []string, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = logs.writer(name)
	cmd.Stderr = logs.writer(name)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return cmd
}

// stackOptions configures newStack.
type stackOptions struct {
	// agents is the number of enrolled devices; zero means one.
	agents int
	// objects runs the fake S3 store and configures snapshot export.
	objects bool
}

// agentProc is one enrolled agent process.
type agentProc struct {
	deviceID string
	stateDir string
}

// newStack starts the control plane, creates an organization with a user
// token, enrolls and starts the agents with the process runtime, sets
// device limits and creates an API key.
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

	addr := freeTCPPort(t)
	stunAddr := freeUDPPort(t)
	s := &stack{t: t, bins: bins, apiURL: "http://" + addr, stunAddr: stunAddr, key: key, userSub: "e2e-user-" + randomSuffix(t), logs: logs}
	apiEnv := []string{
		"ADDR=" + addr,
		"LOG_LEVEL=debug",
		"OWNSAND_DATABASE_URL=" + dbtest.NewURL(t),
		"OWNSAND_LOGTO_ISSUER=" + testIssuer,
		"OWNSAND_LOGTO_JWKS_FILE=" + jwksFile,
		"OWNSAND_STUN_ADDR=" + stunAddr,
		"OWNSAND_STUN_URLS=stun:" + stunAddr,
	}
	if o.objects {
		s.objects = newFakeS3(t)
		apiEnv = append(apiEnv,
			"OWNSAND_R2_ENDPOINT="+s.objects.url,
			"OWNSAND_R2_BUCKET="+fakeBucket,
			"OWNSAND_R2_ACCESS_KEY_ID=e2e-access-key",
			"OWNSAND_R2_SECRET_ACCESS_KEY=e2e-secret-key",
			// The minimum part size, so a snapshot of a few MiB has
			// several parts.
			"OWNSAND_SNAPSHOT_PART_SIZE_BYTES=5242880",
		)
	}
	startProcess(t, logs, "api", apiEnv, filepath.Join(bins, "ownsand-api"))
	s.waitHealthy()

	ctx := s.ctx(2 * time.Minute)
	user := s.userClient(ownsand.TransportAuto)
	org, err := user.Organizations().Create(ctx, &ownsandv1.CreateOrganizationRequest{Name: "e2e"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	s.orgID = org.GetOrganization().GetOrganizationId()
	user = s.userClient(ownsand.TransportAuto)

	for i := range o.agents {
		s.agents = append(s.agents, s.startAgent(ctx, user, work, i))
	}
	s.deviceID = s.agents[0].deviceID
	k, err := user.APIKeys().Create(ctx, &ownsandv1.CreateApiKeyRequest{OrganizationId: s.orgID, Name: "e2e"})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	s.apiKey = k.GetApiKeySecret()
	return s
}

// startAgent enrolls agent i with a new enrollment token, runs it, waits
// until its device is online and sets its limits.
func (s *stack) startAgent(ctx context.Context, user *ownsand.Client, work string, i int) agentProc {
	t := s.t
	t.Helper()
	tok, err := user.Devices().CreateEnrollmentToken(ctx, &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: s.orgID, Ttl: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatalf("CreateEnrollmentToken: %v", err)
	}
	stateDir := filepath.Join(work, fmt.Sprintf("agent-%d", i))
	agentEnv := []string{
		"OWNSAND_AGENT_STATE_DIR=" + stateDir,
		"OWNSAND_AGENT_RUNTIME=process",
		"OWNSAND_AGENT_GUEST_BINARY=" + filepath.Join(s.bins, "ownsand-guest"),
		"OWNSAND_AGENT_LOG_LEVEL=debug",
		"NO_COLOR=1",
	}
	enroll := exec.CommandContext(ctx, filepath.Join(s.bins, "ownsand-agent"), "enroll", "--api-url", s.apiURL, "--name", fmt.Sprintf("e2e-device-%d", i))
	enroll.Env = append(os.Environ(), agentEnv...)
	enroll.Stdin = strings.NewReader(tok.GetEnrollmentToken() + "\n")
	if out, err := enroll.CombinedOutput(); err != nil {
		t.Fatalf("ownsand-agent enroll: %v\n%s", err, out)
	}
	startProcess(t, s.logs, fmt.Sprintf("agent%d", i), agentEnv, filepath.Join(s.bins, "ownsand-agent"), "run")

	known := map[string]bool{}
	for _, a := range s.agents {
		known[a.deviceID] = true
	}
	deviceID := s.waitDeviceOnline(user, known)
	if _, err := user.Devices().UpdateLimits(ctx, &ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: s.orgID, DeviceId: deviceID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 100, VcpuOvercommitPercent: 100}}); err != nil {
		t.Fatalf("UpdateDeviceLimits: %v", err)
	}
	return agentProc{deviceID: deviceID, stateDir: stateDir}
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

func (s *stack) waitHealthy() {
	s.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.apiURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("the control plane did not become healthy")
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

// waitDeviceOnline returns the ID of an online device that is not in skip.
func (s *stack) waitDeviceOnline(c *ownsand.Client, skip map[string]bool) string {
	s.t.Helper()
	ctx := s.ctx(time.Minute)
	for {
		res, err := c.Devices().List(ctx, &ownsandv1.ListDevicesRequest{OrganizationId: s.orgID})
		if err != nil {
			s.t.Fatalf("ListDevices: %v", err)
		}
		for _, d := range res.GetDevices() {
			if d.GetState() == ownsandv1.DeviceState_DEVICE_STATE_ONLINE && !skip[d.GetDeviceId()] {
				return d.GetDeviceId()
			}
		}
		select {
		case <-ctx.Done():
			s.t.Fatal("the device did not come online")
		case <-time.After(200 * time.Millisecond):
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
