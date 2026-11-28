// SPDX-License-Identifier: Apache-2.0

package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/pins"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

const (
	testVersion = "24.22.3"
	adapterName = "@agentclientprotocol/claude-agent-acp"
	agentName   = "@anthropic-ai/claude-agent-sdk"
)

var claude = sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE

// archiveFile is one entry of a fake runtime archive.
type archiveFile struct {
	name, body, link string
}

// symlinksSupported reports whether this platform and account can create
// symbolic links (Windows needs a privilege or developer mode).
func symlinksSupported(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	return os.Symlink("target", filepath.Join(dir, "probe")) == nil
}

// nodeTarGz builds a Node.js-like archive. Like the official archive, it
// links bin/npm to the npm script when the platform supports links.
func nodeTarGz(t *testing.T, root string, extra ...archiveFile) []byte {
	t.Helper()
	files := []archiveFile{
		{name: root + "/bin/node", body: "#!/bin/sh\n"},
		{name: root + "/lib/node_modules/npm/bin/npm-cli.js", body: "// npm\n"},
	}
	if symlinksSupported(t) {
		files = append(files, archiveFile{name: root + "/bin/npm", link: "../lib/node_modules/npm/bin/npm-cli.js"})
	}
	files = append(files, extra...)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		header := &tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if f.link != "" {
			header = &tar.Header{Name: f.name, Linkname: f.link, Typeflag: tar.TypeSymlink, Mode: 0o777}
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if f.link == "" {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func nodeZip(t *testing.T, root string, extra ...archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := append([]archiveFile{
		{name: root + "/node.exe", body: "MZ"},
		{name: root + "/node_modules/npm/bin/npm-cli.js", body: "// npm\n"},
	}, extra...)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// server serves archive at /node and counts requests. failures makes the
// first requests fail with 503.
type server struct {
	*httptest.Server
	requests atomic.Int32
	failures int32
	body     []byte
}

func newServer(t *testing.T, body []byte, failures int32) *server {
	s := &server{body: body, failures: failures}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.requests.Add(1)
		if n <= s.failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(s.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func testPins(t *testing.T, url, sha string) *pins.Pins {
	t.Helper()
	data := fmt.Sprintf(`{
  "schemaVersion": 1,
  "runtimes": {"node": {"version": %[1]q, "checksumsVersion": %[1]q, "reportedVersion": "v%[1]s", "targets": {
    "linux-amd64": {"url": %[2]q, "sha256": %[3]q, "format": "tar.gz", "root": "node-linux", "executable": "bin/node", "npmCli": "lib/node_modules/npm/bin/npm-cli.js", "globalModules": "lib/node_modules"},
    "windows-amd64": {"url": %[2]q, "sha256": %[3]q, "format": "zip", "root": "node-win", "executable": "node.exe", "npmCli": "node_modules/npm/bin/npm-cli.js", "globalModules": "node_modules"}
  }}},
  "vendors": {"AGENT_VENDOR_CLAUDE_CODE": {"runtime": "node", "packageDir": "claude-code",
    "adapter": {"name": %[4]q, "version": "0.113.0", "entry": "dist/index.js"},
    "agent": {"name": "@anthropic-ai/claude-agent-sdk", "version": "0.3.331"},
    "targets": ["linux-amd64", "windows-amd64"]}}
}`, testVersion, url, sha, adapterName)
	p, err := pins.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeRunner answers `node --version` and simulates `npm ci`.
type fakeRunner struct {
	mu          sync.Mutex
	version     string
	npmOutputs  []string // per npm run: "" succeeds, other text fails
	npmRuns     [][]string
	npmEnv      [][]string
	versionRuns int
}

func (r *fakeRunner) Run(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Equal(args, []string{"--version"}) {
		r.versionRuns++
		if r.version == "" {
			return nil, errors.New("not found")
		}
		return []byte(r.version + "\n"), nil
	}
	r.npmRuns = append(r.npmRuns, append([]string{name}, args...))
	r.npmEnv = append(r.npmEnv, env)
	output := ""
	if i := len(r.npmRuns) - 1; i < len(r.npmOutputs) {
		output = r.npmOutputs[i]
	}
	if output != "" {
		return []byte(output), errors.New("exit status 1")
	}
	entry := filepath.Join(dir, "node_modules", filepath.FromSlash(adapterName), "dist", "index.js")
	if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(entry, []byte("// adapter\n"), 0o600)
}

func newInstaller(t *testing.T, p *pins.Pins, target pins.Target, runner *fakeRunner) *Installer {
	return &Installer{
		AgentsDir:  filepath.Join(t.TempDir(), "agents"),
		Target:     target,
		Pins:       p,
		Runner:     runner,
		LookPath:   func(string) (string, error) { return "", errors.New("not found") },
		Environ:    func() []string { return []string{"PATH=/usr/bin"} },
		BackoffMin: time.Millisecond,
		BackoffMax: 2 * time.Millisecond,
	}
}

func TestInstallsRuntimeAndAdapter(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 0)
	runner := &fakeRunner{}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, runner)
	result, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(installer.AgentsDir, "node", testVersion, "node-linux")
	if result.RuntimePath != filepath.Join(runtimeRoot, "bin", "node") || result.RuntimePreinstalled || result.AdapterPreinstalled {
		t.Fatalf("result %+v", result)
	}
	pkgRoot := filepath.Join(installer.AgentsDir, "claude-code", "0.113.0")
	if result.AdapterEntry != filepath.Join(pkgRoot, "node_modules", filepath.FromSlash(adapterName), "dist", "index.js") {
		t.Fatalf("adapter entry %s", result.AdapterEntry)
	}
	if symlinksSupported(t) {
		if target, err := os.Readlink(filepath.Join(runtimeRoot, "bin", "npm")); err != nil || target != filepath.FromSlash("../lib/node_modules/npm/bin/npm-cli.js") {
			t.Fatalf("npm link %q %v", target, err)
		}
	}
	_, lock, _ := pins.NpmFiles("claude-code")
	if written, _ := os.ReadFile(filepath.Join(pkgRoot, "package-lock.json")); !bytes.Equal(written, lock) {
		t.Fatal("the embedded lockfile was not written")
	}
	if len(runner.npmRuns) != 1 {
		t.Fatalf("npm runs %v", runner.npmRuns)
	}
	run := runner.npmRuns[0]
	if run[0] != result.RuntimePath || run[1] != filepath.Join(runtimeRoot, "lib", "node_modules", "npm", "bin", "npm-cli.js") || run[2] != "ci" || !slices.Contains(run, "--ignore-scripts") {
		t.Fatalf("npm command %v", run)
	}
	if !slices.Contains(runner.npmEnv[0], "npm_config_cache="+filepath.Join(installer.AgentsDir, "npm-cache")) {
		t.Fatalf("npm environment %v", runner.npmEnv[0])
	}

	// A second start reuses both installations.
	again, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	if again != result || srv.requests.Load() != 1 || len(runner.npmRuns) != 1 {
		t.Fatalf("second install %+v, %d downloads, %d npm runs", again, srv.requests.Load(), len(runner.npmRuns))
	}
	if leftovers, _ := filepath.Glob(filepath.Join(installer.AgentsDir, "node", ".*")); len(leftovers) != 0 {
		t.Fatalf("temporary files remain: %v", leftovers)
	}
}

func TestInstallsZipRuntime(t *testing.T) {
	archive := nodeZip(t, "node-win")
	srv := newServer(t, archive, 0)
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetWindowsAMD64, &fakeRunner{})
	result, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	if result.RuntimePath != filepath.Join(installer.AgentsDir, "node", testVersion, "node-win", "node.exe") {
		t.Fatalf("runtime %s", result.RuntimePath)
	}
	if data, err := os.ReadFile(result.RuntimePath); err != nil || string(data) != "MZ" {
		t.Fatalf("node.exe %q %v", data, err)
	}
}

func installKind(t *testing.T, err error) sandcrewv1.BridgeErrorKind {
	t.Helper()
	installErr, ok := IsError(err)
	if !ok {
		t.Fatalf("error %v is no install error", err)
	}
	return installErr.Kind
}

func TestChecksumMismatchFailsWithoutRetry(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 0)
	runner := &fakeRunner{}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", strings.Repeat("0", 64)), pins.TargetLinuxAMD64, runner)
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CHECKSUM_MISMATCH {
		t.Fatalf("kind %s", kind)
	}
	if srv.requests.Load() != 1 || len(runner.npmRuns) != 0 {
		t.Fatalf("%d downloads, %d npm runs", srv.requests.Load(), len(runner.npmRuns))
	}
	if _, err := os.Stat(filepath.Join(installer.AgentsDir, "node", testVersion)); !os.IsNotExist(err) {
		t.Fatal("an unverified archive was extracted")
	}
}

func TestDownloadFailsAfterThreeAttempts(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 100)
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, &fakeRunner{})
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED {
		t.Fatalf("kind %s", kind)
	}
	if srv.requests.Load() != Attempts || !strings.Contains(err.Error(), "503") {
		t.Fatalf("%d downloads, error %v", srv.requests.Load(), err)
	}
}

func TestDownloadRetriesATransientFailure(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 2)
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, &fakeRunner{})
	if _, err := installer.Install(context.Background(), claude); err != nil {
		t.Fatal(err)
	}
	if srv.requests.Load() != 3 {
		t.Fatalf("%d downloads", srv.requests.Load())
	}
}

func TestDownloadBudgetLimitsTheAttempts(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 100)
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, &fakeRunner{})
	installer.BackoffMin, installer.BackoffMax = time.Hour, time.Hour
	installer.Budget = 50 * time.Millisecond
	started := time.Now()
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED {
		t.Fatalf("kind %s", kind)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("the budget did not stop the backoff")
	}
}

func TestArchiveEntriesMustStayInside(t *testing.T) {
	for name, archive := range map[string]func(*testing.T) ([]byte, pins.Target){
		"tar": func(t *testing.T) ([]byte, pins.Target) {
			return nodeTarGz(t, "node-linux", archiveFile{name: "../escape", body: "x"}), pins.TargetLinuxAMD64
		},
		"tar link": func(t *testing.T) ([]byte, pins.Target) {
			return nodeTarGz(t, "node-linux", archiveFile{name: "node-linux/evil", link: "../../../../etc/passwd"}), pins.TargetLinuxAMD64
		},
		"zip": func(t *testing.T) ([]byte, pins.Target) {
			return nodeZip(t, "node-win", archiveFile{name: "../escape", body: "x"}), pins.TargetWindowsAMD64
		},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "tar link" && !symlinksSupported(t) {
				t.Skip("the platform cannot create symbolic links")
			}
			data, target := archive(t)
			srv := newServer(t, data, 0)
			installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(data)), target, &fakeRunner{})
			_, err := installer.Install(context.Background(), claude)
			if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED {
				t.Fatalf("kind %s", kind)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(installer.AgentsDir), "escape")); !os.IsNotExist(err) {
				t.Fatal("an archive entry left the target directory")
			}
		})
	}
}

func TestNpmIntegrityMismatchIsAChecksumMismatch(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 0)
	runner := &fakeRunner{npmOutputs: []string{"npm error code EINTEGRITY\nnpm error sha512-... integrity checksum failed"}}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, runner)
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CHECKSUM_MISMATCH {
		t.Fatalf("kind %s", kind)
	}
	if len(runner.npmRuns) != 1 {
		t.Fatalf("npm runs %d", len(runner.npmRuns))
	}
}

func TestNpmFailureFailsAfterThreeAttempts(t *testing.T) {
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 0)
	runner := &fakeRunner{npmOutputs: []string{"npm error network ECONNRESET", "npm error network ECONNRESET", "npm error network ECONNRESET"}}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, runner)
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED {
		t.Fatalf("kind %s", kind)
	}
	if len(runner.npmRuns) != Attempts || !strings.Contains(err.Error(), "ECONNRESET") {
		t.Fatalf("npm runs %d, error %v", len(runner.npmRuns), err)
	}
	// A later start tries again: no marker was written.
	runner.npmOutputs = nil
	runner.npmRuns = nil
	if _, err := installer.Install(context.Background(), claude); err != nil {
		t.Fatal(err)
	}
}

// preinstalledNode creates a Node.js prefix like an image would have, with
// the adapter and its agent installed globally (the agent hoisted next to
// the adapter). An empty version leaves the package out.
func preinstalledNode(t *testing.T, adapterVersion, agentVersion string) string {
	t.Helper()
	prefix := t.TempDir()
	for _, file := range []string{"bin/node", "lib/node_modules/npm/bin/npm-cli.js"} {
		path := filepath.Join(prefix, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if adapterVersion != "" {
		writePackage(t, filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(adapterName)), adapterVersion)
	}
	if agentVersion != "" {
		writePackage(t, filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(agentName)), agentVersion)
	}
	return prefix
}

func writePackage(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+version+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreinstalledComponentsWithThePinnedVersion(t *testing.T) {
	prefix := preinstalledNode(t, "0.113.0", "0.3.331")
	srv := newServer(t, nil, 100)
	runner := &fakeRunner{version: "v" + testVersion}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", strings.Repeat("0", 64)), pins.TargetLinuxAMD64, runner)
	installer.LookPath = func(name string) (string, error) {
		if name != "node" {
			return "", errors.New("not found")
		}
		return filepath.Join(prefix, "bin", "node"), nil
	}
	result, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(prefix)
	if !result.RuntimePreinstalled || !result.AdapterPreinstalled || result.RuntimePath != filepath.Join(resolved, "bin", "node") ||
		result.AdapterEntry != filepath.Join(resolved, "lib", "node_modules", filepath.FromSlash(adapterName), "dist", "index.js") {
		t.Fatalf("result %+v", result)
	}
	if srv.requests.Load() != 0 || len(runner.npmRuns) != 0 {
		t.Fatalf("%d downloads, %d npm runs", srv.requests.Load(), len(runner.npmRuns))
	}
}

func TestPreinstalledRuntimeWithAnotherAdapterVersion(t *testing.T) {
	prefix := preinstalledNode(t, "0.100.0", "0.3.331")
	srv := newServer(t, nil, 100)
	runner := &fakeRunner{version: "v" + testVersion}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", strings.Repeat("0", 64)), pins.TargetLinuxAMD64, runner)
	installer.LookPath = func(string) (string, error) { return filepath.Join(prefix, "bin", "node"), nil }
	result, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RuntimePreinstalled || result.AdapterPreinstalled || len(runner.npmRuns) != 1 || srv.requests.Load() != 0 {
		t.Fatalf("result %+v, %d npm runs, %d downloads", result, len(runner.npmRuns), srv.requests.Load())
	}
}

func TestPreinstalledAdapterWithAnotherAgentVersion(t *testing.T) {
	cases := map[string]func(prefix string){
		// The hoisted agent has another patch version.
		"hoisted": func(prefix string) {
			writePackage(t, filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(agentName)), "0.3.332")
		},
		// A nested copy wins over the matching hoisted one.
		"nested": func(prefix string) {
			writePackage(t, filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(adapterName), "node_modules", filepath.FromSlash(agentName)), "0.3.330")
		},
		// No agent package at all.
		"missing": func(prefix string) {
			if err := os.RemoveAll(filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(agentName))); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			prefix := preinstalledNode(t, "0.113.0", "0.3.331")
			change(prefix)
			srv := newServer(t, nil, 100)
			runner := &fakeRunner{version: "v" + testVersion}
			installer := newInstaller(t, testPins(t, srv.URL+"/node", strings.Repeat("0", 64)), pins.TargetLinuxAMD64, runner)
			installer.LookPath = func(string) (string, error) { return filepath.Join(prefix, "bin", "node"), nil }
			result, err := installer.Install(context.Background(), claude)
			if err != nil {
				t.Fatal(err)
			}
			if !result.RuntimePreinstalled || result.AdapterPreinstalled || len(runner.npmRuns) != 1 {
				t.Fatalf("result %+v, %d npm runs; want the locked adapter installation", result, len(runner.npmRuns))
			}
		})
	}
}

func TestPreinstalledRuntimeWithAnotherVersionIsNotUsed(t *testing.T) {
	prefix := preinstalledNode(t, "0.113.0", "0.3.331")
	archive := nodeTarGz(t, "node-linux")
	srv := newServer(t, archive, 0)
	runner := &fakeRunner{version: "v22.0.0"}
	installer := newInstaller(t, testPins(t, srv.URL+"/node", digest(archive)), pins.TargetLinuxAMD64, runner)
	installer.LookPath = func(string) (string, error) { return filepath.Join(prefix, "bin", "node"), nil }
	result, err := installer.Install(context.Background(), claude)
	if err != nil {
		t.Fatal(err)
	}
	if result.RuntimePreinstalled || result.AdapterPreinstalled || srv.requests.Load() != 1 || runner.versionRuns != 1 {
		t.Fatalf("result %+v, %d downloads", result, srv.requests.Load())
	}
}

func TestUnsupportedTargetFails(t *testing.T) {
	installer := newInstaller(t, testPins(t, "https://example.invalid/node", strings.Repeat("0", 64)), pins.TargetDarwinARM64, &fakeRunner{})
	_, err := installer.Install(context.Background(), claude)
	if kind := installKind(t, err); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED {
		t.Fatalf("kind %s", kind)
	}
}
