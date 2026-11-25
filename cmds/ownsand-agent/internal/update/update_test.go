// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type release struct {
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	manifest []byte
	sig      []byte
	artifact []byte
	srv      *httptest.Server
}

func newRelease(t *testing.T, mutate func(*Manifest, *[]byte)) *release {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &release{pub: pub, priv: priv, artifact: []byte("new agent binary")}
	mux := http.NewServeMux()
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	sum := sha256.Sum256(r.artifact)
	m := Manifest{Schema: 1, Releases: []Release{{Version: "1.1.0", Default: true, Artifacts: []Artifact{{Platform: "linux-amd64", URL: r.srv.URL + "/agent", SHA256: hex.EncodeToString(sum[:])}}}}, RevokedVersions: []string{"0.9.0"}}
	artifact := r.artifact
	if mutate != nil {
		mutate(&m, &artifact)
	}
	r.manifest, _ = json.Marshal(m)
	r.sig = ed25519.Sign(priv, r.manifest)
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(r.manifest) })
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(base64.StdEncoding.EncodeToString(r.sig)))
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) { w.Write(artifact) })
	return r
}

type fakeInstaller struct {
	installed atomic.Int32
	err       error
}

func (f *fakeInstaller) Install(string) error { f.installed.Add(1); return f.err }

func newUpdater(t *testing.T, r *release, running string, busy *atomic.Bool, keys ...ed25519.PublicKey) (*Updater, *fakeInstaller, *atomic.Int32) {
	st, _ := store.Open(t.TempDir())
	inst := &fakeInstaller{}
	restarts := &atomic.Int32{}
	if keys == nil {
		keys = []ed25519.PublicKey{r.pub}
	}
	u, err := New(Options{
		Store: st, HTTP: r.srv.Client(), ManifestURL: r.srv.URL + "/manifest.json", Keys: keys,
		Platform: "linux-amd64", Running: running, Installer: inst,
		Busy: busy.Load, Restart: func() { restarts.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, inst, restarts
}

func update(u *Updater, target string, recovery bool) error {
	return u.Handle(context.Background(), &ownsandv1.UpdateAgentCommand{TargetVersion: target, RevocationRecovery: recovery})
}

func TestUpdateInstallsWhenIdle(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("state %v %s", s, target)
	}
	// Never installs while sandboxes run.
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
		t.Fatal("installed while busy")
	}
	// Creates and the installation are mutually exclusive.
	busy.Store(false)
	release, err := u.BeginCreate()
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := u.TryInstall(); ok {
		t.Fatal("installed during a create")
	}
	release()
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("install: %v %v", ok, err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING {
		t.Fatalf("state %v", s)
	}
	if _, err := u.BeginCreate(); !errors.Is(err, ErrInstalling) {
		t.Fatalf("create during install: %v", err)
	}
	// The next process confirms the installation.
	u2, err := New(Options{Store: u.opts.Store, Running: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := u2.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE {
		t.Fatalf("after restart: %v", s)
	}
}

func TestUpdateRejectsBadReleases(t *testing.T) {
	busy := &atomic.Bool{}
	cases := []struct {
		name   string
		r      *release
		target string
		keys   []ed25519.PublicKey
		want   error
	}{
		{"bad signature", newRelease(t, nil), "1.1.0", []ed25519.PublicKey{func() ed25519.PublicKey { p, _, _ := ed25519.GenerateKey(rand.Reader); return p }()}, ErrBadSignature},
		{"hash mismatch", newRelease(t, func(_ *Manifest, a *[]byte) { *a = []byte("tampered") }), "1.1.0", nil, ErrHashMismatch},
		{"revoked target", newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = append(m.RevokedVersions, "1.1.0") }), "1.1.0", nil, ErrRevoked},
		{"no artifact", newRelease(t, nil), "1.2.0", nil, ErrNoArtifact},
	}
	for _, c := range cases {
		u, inst, _ := newUpdater(t, c.r, "1.0.0", busy, c.keys...)
		if err := update(u, c.target, false); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
			t.Errorf("%s: state %v", c.name, s)
		}
		u.TryInstall()
		if inst.installed.Load() != 0 {
			t.Errorf("%s: installed", c.name)
		}
	}
	r := newRelease(t, nil)
	u, _, _ := newUpdater(t, r, "1.0.0", busy, []ed25519.PublicKey{}...)
	u.opts.Keys = nil
	// The agent has a built-in release key; remove it for this case, which
	// covers an agent with neither a built-in nor a configured key.
	builtin := BuiltinKeys
	BuiltinKeys = nil
	t.Cleanup(func() { BuiltinKeys = builtin })
	if err := update(u, "1.1.0", false); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("no keys: %v", err)
	}
}

func TestRevokedRunningVersion(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, _ := newUpdater(t, r, "0.9.0", busy)
	if err := update(u, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
		t.Fatalf("state %v", s)
	}
	if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("create on revoked version: %v", err)
	}
	busy.Store(false)
	if ok, _ := u.TryInstall(); !ok || inst.installed.Load() != 1 {
		t.Fatal("revoked version not replaced when idle")
	}
}

func TestFailedInstallKeepsRunningRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, running string
		recovery      bool
		wantRevoked   bool
	}{
		{"revoked running version stays blocked", "0.9.0", true, true},
		{"non-revoked running version stays admitted", "1.0.0", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			busy := &atomic.Bool{}
			u, inst, restarts := newUpdater(t, r, tc.running, busy)
			inst.err = errors.New("read-only executable directory")
			if err := update(u, "1.1.0", tc.recovery); err != nil {
				t.Fatal(err)
			}
			if ok, err := u.TryInstall(); ok || err == nil {
				t.Fatalf("install should fail: %v %v", ok, err)
			}
			if restarts.Load() != 0 {
				t.Fatal("restarted after a failed install")
			}
			// A revoked running version stays REVOKED_PENDING_IDLE after the
			// failed install, as after a failed download, so the heartbeat keeps
			// the control plane from placing sandboxes on the device.
			wantState := ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED
			if tc.wantRevoked {
				wantState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
			}
			if s, _ := u.State(); s != wantState {
				t.Fatalf("state %v, want %v", s, wantState)
			}
			release, err := u.BeginCreate()
			if tc.wantRevoked {
				if !errors.Is(err, ErrRunningRevoked) {
					t.Fatalf("create after failed install on revoked version: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("create after failed install: %v", err)
			}
			release()
		})
	}
}

func TestFailedInstallReleasesInstallingFlag(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	inst.err = errors.New("boom")
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	u.TryInstall()
	if _, err := u.BeginCreate(); errors.Is(err, ErrInstalling) {
		t.Fatal("installing flag not released")
	}
}

func TestPinnedLowerVersionAndSameVersion(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	u, inst, _ := newUpdater(t, r, "1.2.0", busy)
	if err := update(u, "1.2.0", false); err != nil {
		t.Fatal(err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE {
		t.Fatalf("same version: %v", s)
	}
	// A pinned lower version that the control plane sends installs.
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := u.TryInstall(); !ok || inst.installed.Load() != 1 {
		t.Fatal("pinned lower version not installed")
	}
}

func TestExecutableInstaller(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ownsand-agent")
	os.WriteFile(exe, []byte("old"), 0o755)
	art := filepath.Join(dir, "artifact")
	os.WriteFile(art, []byte("new"), 0o600)
	if err := (ExecutableInstaller{Path: exe}).Install(art); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("executable %q", b)
	}
}

// renameCall records one rename that the installer asked the seam to run.
type renameCall struct{ from, to string }

// installFixture creates an executable with the old bytes and an artifact with
// the new bytes.
func installFixture(t *testing.T) (exe, art string) {
	t.Helper()
	dir := t.TempDir()
	exe = filepath.Join(dir, "ownsand-agent")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	art = filepath.Join(dir, "artifact")
	if err := os.WriteFile(art, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	return exe, art
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExecutableInstallerWindowsRenames drives the Windows rename sequence
// through the rename seam. The seam performs the real rename unless a case
// makes that call fail, so the files on disk show the outcome on any host.
func TestExecutableInstallerWindowsRenames(t *testing.T) {
	errSecond := errors.New("second rename denied")
	errRestore := errors.New("restore rename denied")
	cases := []struct {
		name string
		// fail returns the injected error for the n-th call (0-based), or nil to
		// run the real rename.
		fail          func(n int, from, to string) error
		wantErr       error
		wantErrText   string
		wantNotRestor bool
		wantCalls     int
		wantExe       string
	}{
		{
			name:      "success",
			fail:      func(int, string, string) error { return nil },
			wantCalls: 2,
			wantExe:   "new",
		},
		{
			name: "first rename fails",
			fail: func(n int, _, _ string) error {
				if n == 0 {
					return errSecond
				}
				return nil
			},
			wantErr:   errSecond,
			wantCalls: 1,
			wantExe:   "old",
		},
		{
			name: "second rename fails and the restore succeeds",
			fail: func(n int, _, _ string) error {
				if n == 1 {
					return errSecond
				}
				return nil
			},
			wantErr:   errSecond,
			wantCalls: 3,
			wantExe:   "old",
		},
		{
			name: "second rename and the restore fail",
			fail: func(n int, _, _ string) error {
				switch n {
				case 1:
					return errSecond
				case 2:
					return errRestore
				}
				return nil
			},
			wantErr:       errSecond,
			wantErrText:   "restore",
			wantNotRestor: true,
			wantCalls:     3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exe, art := installFixture(t)
			var calls []renameCall
			installer := ExecutableInstaller{
				Path: exe,
				goos: "windows",
				rename: func(from, to string) error {
					n := len(calls)
					calls = append(calls, renameCall{from, to})
					if err := tc.fail(n, from, to); err != nil {
						return err
					}
					return os.Rename(from, to)
				},
			}
			err := installer.Install(art)
			if tc.wantErr == nil && tc.wantErrText == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("Install returned nil")
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("error %v does not wrap %v", err, tc.wantErr)
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErrText)
				}
				if tc.wantNotRestor && !strings.Contains(err.Error(), errRestore.Error()) {
					t.Fatalf("error %q does not include the restore failure", err)
				}
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("rename calls %v, want %d", calls, tc.wantCalls)
			}
			old := exe + ".old"
			next := exe + ".new"
			if calls[0] != (renameCall{exe, old}) {
				t.Fatalf("first rename %v", calls[0])
			}
			if tc.wantCalls >= 2 && calls[1] != (renameCall{next, exe}) {
				t.Fatalf("second rename %v", calls[1])
			}
			if tc.wantCalls == 3 && calls[2] != (renameCall{old, exe}) {
				t.Fatalf("restore rename %v", calls[2])
			}
			if tc.wantNotRestor {
				// The restore failed, so Path stays missing and the previous bytes
				// remain at Path.old.
				if _, statErr := os.Stat(exe); !os.IsNotExist(statErr) {
					t.Fatalf("executable stat error %v, want not exist", statErr)
				}
				if got := readString(t, old); got != "old" {
					t.Fatalf("previous executable %q", got)
				}
				return
			}
			if got := readString(t, exe); got != tc.wantExe {
				t.Fatalf("executable %q, want %q", got, tc.wantExe)
			}
		})
	}
}

// TestExecutableInstallerNonWindowsSingleRename checks that other hosts keep
// one rename of Path.new onto Path and never create Path.old.
func TestExecutableInstallerNonWindowsSingleRename(t *testing.T) {
	exe, art := installFixture(t)
	var calls []renameCall
	installer := ExecutableInstaller{
		Path: exe,
		goos: "linux",
		rename: func(from, to string) error {
			calls = append(calls, renameCall{from, to})
			return os.Rename(from, to)
		},
	}
	if err := installer.Install(art); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (renameCall{exe + ".new", exe}) {
		t.Fatalf("rename calls %v", calls)
	}
	if got := readString(t, exe); got != "new" {
		t.Fatalf("executable %q", got)
	}
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("unexpected .old file: %v", err)
	}
}

// TestFailedWindowsRenameRestoresExecutableAndSkipsRestart runs TryInstall with
// the real ExecutableInstaller on the Windows rename sequence, where the rename
// of the new executable onto Path fails.
func TestFailedWindowsRenameRestoresExecutableAndSkipsRestart(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	u, _, restarts := newUpdater(t, r, "1.0.0", busy)
	exe, _ := installFixture(t)
	renames := 0
	u.opts.Installer = ExecutableInstaller{
		Path: exe,
		goos: "windows",
		rename: func(from, to string) error {
			renames++
			if from == exe+".new" {
				return errors.New("rename denied")
			}
			return os.Rename(from, to)
		},
	}
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if ok, err := u.TryInstall(); ok || err == nil {
		t.Fatalf("install should fail: %v %v", ok, err)
	}
	if renames != 3 {
		t.Fatalf("rename calls %d, want 3", renames)
	}
	if got := readString(t, exe); got != "old" {
		t.Fatalf("executable %q, want the previous bytes", got)
	}
	if restarts.Load() != 0 {
		t.Fatal("restarted after a failed install")
	}
	if st, _ := u.opts.Store.LoadAgent(); st != nil && st.PendingUpdate != nil {
		t.Fatalf("pending update recorded: %+v", st.PendingUpdate)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
}

func TestSupersedingCommandClearsQueuedArtifact(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		wantErr   bool
		wantState ownsandv1.AgentUpdateState
	}{
		{"same version", "1.0.0", false, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE},
		{"failed target", "9.9.9", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"invalid target", "not-semver", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"zero-padded target", "01.1.0", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"signed target", "+1.1.0", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"empty pre-release target", "1.1.0-", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			busy := &atomic.Bool{}
			busy.Store(true)
			u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
			if err := update(u, "1.1.0", false); err != nil {
				t.Fatal(err)
			}
			if err := update(u, tc.target, false); (err != nil) != tc.wantErr {
				t.Fatalf("superseding command: %v", err)
			}
			if s, _ := u.State(); s != tc.wantState {
				t.Fatalf("state %v", s)
			}
			busy.Store(false)
			if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
				t.Fatalf("installed a superseded artifact: %v %v", ok, err)
			}
			if st, _ := u.opts.Store.LoadAgent(); st != nil && st.PendingUpdate != nil {
				t.Fatalf("pending update recorded: %+v", st.PendingUpdate)
			}
			// Control: a valid later target still installs.
			if err := update(u, "1.1.0", false); err != nil {
				t.Fatal(err)
			}
			if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 {
				t.Fatalf("control install: %v %v", ok, err)
			}
		})
	}
}

// gateTimeout bounds waits on gated downloads and installs, so a regression or an
// earlier failure reports a diagnostic instead of hanging the test binary.
const gateTimeout = 10 * time.Second

// gateTransport holds a download until open is called or the request
// context ends, and signals started when that download begins.
type gateTransport struct {
	base    http.RoundTripper
	path    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// newGateTransport returns a gate that is opened when the test ends, so a
// fatal assertion cannot leave a download blocked.
func newGateTransport(t *testing.T, base http.RoundTripper) *gateTransport {
	t.Helper()
	g := &gateTransport{base: base, path: "/agent", started: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

// open releases the held download. It is safe to call more than once.
func (g *gateTransport) open() {
	g.once.Do(func() { close(g.release) })
}

func (g *gateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == g.path {
		select {
		case g.started <- struct{}{}:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		select {
		case <-g.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return g.base.RoundTrip(req)
}

// awaitStarted fails the test when the gated download does not begin in time.
func (g *gateTransport) awaitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(gateTimeout):
		t.Fatalf("download did not start within %v", gateTimeout)
	}
}

// awaitDone returns the result of the command goroutine, and fails the test
// when it does not finish in time.
func awaitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(gateTimeout):
		t.Fatalf("download did not finish within %v", gateTimeout)
		return nil
	}
}

func TestWithdrawWhilePendingIdleNeverInstalls(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE {
		t.Fatalf("state %v", s)
	}
	u.mu.Lock()
	artifact := u.artifact
	u.mu.Unlock()
	if err := u.Withdraw(); err != nil {
		t.Fatal(err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE || target != "" {
		t.Fatalf("state after withdrawal %v %q", s, target)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("queued artifact still on disk: %v", err)
	}
	// The device becomes idle: the installer is never called.
	busy.Store(false)
	if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("installed a withdrawn update: %v %v", ok, err)
	}
	if st, _ := u.opts.Store.LoadAgent(); st != nil && st.PendingUpdate != nil {
		t.Fatalf("pending update recorded: %+v", st.PendingUpdate)
	}
	// A repeated withdrawal with nothing queued succeeds.
	if err := u.Withdraw(); err != nil {
		t.Fatal(err)
	}
	// Control: a later authorized update still installs once idle.
	busy.Store(true)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("control install: %v %v", ok, err)
	}
}

func TestWithdrawDuringDownloadNeverInstalls(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	gate := newGateTransport(t, r.srv.Client().Transport)
	u.opts.HTTP = &http.Client{Transport: gate}

	// The commands take their orders in receive order, as the agent does.
	updateOrder := u.NextOrder()
	// The worker is cancelled and joined when the test ends.
	done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, updateOrder)
	gate.awaitStartedOrEnd(t, done)
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING {
		t.Fatalf("state %v", s)
	}
	if err := u.WithdrawOrdered(u.NextOrder()); err != nil {
		t.Fatal(err)
	}
	gate.open()
	if err := awaitDone(t, done); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("download finished with %v, want ErrSuperseded", err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE || target != "" {
		t.Fatalf("state %v %q", s, target)
	}
	if files, _ := filepath.Glob(filepath.Join(u.opts.Store.Path(store.UpdateDir), "*")); len(files) != 0 {
		t.Fatalf("discarded download left files: %v", files)
	}
	if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("installed after a withdrawn download: %v %v", ok, err)
	}
}

func TestRemovalPreventsLateDownloadFromWritingArtifact(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, restarts := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	gate := newGateTransport(t, r.srv.Client().Transport)
	u.opts.HTTP = &http.Client{Transport: gate}

	done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
	gate.awaitStartedOrEnd(t, done)
	if installing := u.FenceForRemoval(); installing != nil {
		t.Fatal("installation admitted before the artifact was downloaded")
	}
	if err := u.opts.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	// The request stays live: the verified download must hit the fence even
	// after reset has cleared the state.
	gate.open()
	if err := awaitDone(t, done); !errors.Is(err, ErrRemovalAborted) {
		t.Fatalf("download finished with %v, want ErrRemovalAborted", err)
	}
	if entries, err := os.ReadDir(u.opts.Store.Dir()); err != nil || len(entries) != 1 || entries[0].Name() != store.EnrollmentLockFile {
		t.Fatalf("late download modified the state directory: %v, %v", entries, err)
	}
	if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("installed after the download was fenced: %v, %v", ok, err)
	}
}

func TestOlderUpdateCommandLosesToNewerOrder(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, _ := newUpdater(t, r, "1.0.0", busy)
	older, newer := u.NextOrder(), u.NextOrder()
	// The withdrawal arrives first in goroutine scheduling but was received
	// after the update, so it wins and the late update is rejected.
	if err := u.WithdrawOrdered(newer); err != nil {
		t.Fatal(err)
	}
	if err := u.HandleOrdered(context.Background(), &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, older); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("late older update: %v", err)
	}
	busy.Store(false)
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
		t.Fatal("installed a superseded update")
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE {
		t.Fatalf("state %v", s)
	}
}

// An update command with an invalid target follows the same supersession rules
// as every other update command: an older one that runs after a newer valid
// command must not touch the state, the target or the queued artifact of the
// newer command.
func TestOlderInvalidTargetLosesToNewerValidCommand(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	older, newer := u.NextOrder(), u.NextOrder()
	if err := u.HandleOrdered(context.Background(), &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, newer); err != nil {
		t.Fatal(err)
	}
	err := u.HandleOrdered(context.Background(), &ownsandv1.UpdateAgentCommand{TargetVersion: "not-semver"}, older)
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("late older invalid update: %v", err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("state %v %s", s, target)
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("the newer artifact was not installed: %v %v", ok, err)
	}
	st, err := u.opts.Store.LoadAgent()
	if err != nil || st == nil || st.PendingUpdate == nil || st.PendingUpdate.TargetVersion != "1.1.0" {
		t.Fatalf("recorded pending update: %+v %v", st, err)
	}
	// The next process confirms the installation of the newer target.
	u2, err := New(Options{Store: u.opts.Store, Running: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := u2.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE {
		t.Fatalf("after restart: %v", s)
	}
}

func TestWithdrawAfterInstallStartedAndWhileRevoked(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, _ := newUpdater(t, r, "1.0.0", busy)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil {
		t.Fatalf("install: %v %v", ok, err)
	}
	// The installation boundary: it cannot be taken back.
	if err := u.Withdraw(); !errors.Is(err, ErrInstalling) {
		t.Fatalf("withdrawal during installation: %v", err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING || inst.installed.Load() != 1 {
		t.Fatalf("state %v after a late withdrawal", s)
	}

	// A revoked running version keeps its recovery: the withdrawal changes
	// nothing.
	rr := newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = append(m.RevokedVersions, "1.0.0") })
	u2, inst2, _ := newUpdater(t, rr, "1.0.0", busy)
	busy.Store(true)
	if err := update(u2, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	if s, _ := u2.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
		t.Fatalf("state %v", s)
	}
	if err := u2.Withdraw(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("withdrawal while revoked: %v", err)
	}
	busy.Store(false)
	if ok, err := u2.TryInstall(); !ok || err != nil || inst2.installed.Load() != 1 {
		t.Fatalf("recovery install: %v %v", ok, err)
	}
}

// TestWithdrawDuringDownloadKeepsSignedRevocation covers a withdrawal that
// arrives while the artifact downloads, after the signed manifest revoking the
// running version was verified. The server release table may not show that
// revocation yet, so the agent must trust the manifest: the withdrawal changes
// nothing and the recovery update is queued.
func TestWithdrawDuringDownloadKeepsSignedRevocation(t *testing.T) {
	r := newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = append(m.RevokedVersions, "1.0.0") })
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	gate := newGateTransport(t, r.srv.Client().Transport)
	u.opts.HTTP = &http.Client{Transport: gate}

	updateOrder := u.NextOrder()
	// The worker is cancelled and joined when the test ends.
	done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0", RevocationRecovery: true}, updateOrder)
	gate.awaitStartedOrEnd(t, done)
	if err := u.WithdrawOrdered(u.NextOrder()); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("withdrawal during the download of a revocation recovery: %v", err)
	}
	if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("create while the manifest revokes the running version: %v", err)
	}
	// The heartbeat state must agree with the create admission while the
	// artifact is still downloading.
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("state during the download %v %q", s, target)
	}
	gate.open()
	if err := awaitDone(t, done); err != nil {
		t.Fatalf("the recovery download finished with %v", err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("state %v %q", s, target)
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("recovery install: %v %v", ok, err)
	}
}

// TestFailedInstallRemovesTheArtifact checks that a failed installation does
// not leave the downloaded file behind: every command has its own file name,
// so repeated failures would otherwise collect one executable per retry.
func TestFailedInstallRemovesTheArtifact(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	inst.err = errors.New("read-only executable directory")
	files := func() []string {
		names, _ := filepath.Glob(filepath.Join(u.opts.Store.Path(store.UpdateDir), "*"))
		return names
	}
	for i := 0; i < 3; i++ {
		busy := &atomic.Bool{}
		busy.Store(true)
		u.opts.Busy = busy.Load
		if err := update(u, "1.1.0", false); err != nil {
			t.Fatal(err)
		}
		if len(files()) != 1 {
			t.Fatalf("attempt %d: %d files after the download, want 1", i, len(files()))
		}
		busy.Store(false)
		if ok, err := u.TryInstall(); ok || err == nil {
			t.Fatalf("attempt %d: install should fail: %v %v", i, ok, err)
		}
		if names := files(); len(names) != 0 {
			t.Fatalf("attempt %d: the failed installation left %v", i, names)
		}
	}
}

// TestArtifactFailureKeepsSignedRevocation checks that a failed artifact
// download keeps the state REVOKED_PENDING_IDLE while the verified manifest
// revokes the running version, and that a later verified artifact installs.
func TestArtifactFailureKeepsSignedRevocation(t *testing.T) {
	r := newRelease(t, func(_ *Manifest, a *[]byte) { *a = []byte("tampered") })
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, "0.9.0", busy)
	if err := update(u, "1.1.0", true); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("hash mismatch: %v", err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
		t.Fatalf("state after the artifact failure: %v", s)
	}
	if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("create after the artifact failure: %v", err)
	}
	if inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatal("installer or restart ran after a failed artifact")
	}
	// A later command stages a verified artifact and installs once idle.
	good := newRelease(t, nil)
	u.opts.HTTP, u.opts.ManifestURL, u.opts.Keys = good.srv.Client(), good.srv.URL+"/manifest.json", []ed25519.PublicKey{good.pub}
	if err := update(u, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 {
		t.Fatalf("recovery install: %v %v", ok, err)
	}
}

// TestArtifactFailureWithoutRevocationReportsFailed covers the controls: an
// unrevoked running version and an untrusted manifest signature both report
// FAILED and never gate creates.
func TestArtifactFailureWithoutRevocationReportsFailed(t *testing.T) {
	tamper := func(_ *Manifest, a *[]byte) { *a = []byte("tampered") }
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, tc := range []struct {
		name    string
		running string
		keys    func(*release) []ed25519.PublicKey
	}{
		{"running version not revoked", "1.0.0", func(r *release) []ed25519.PublicKey { return []ed25519.PublicKey{r.pub} }},
		{"invalid manifest signature", "0.9.0", func(*release) []ed25519.PublicKey { return []ed25519.PublicKey{other} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, tamper)
			busy := &atomic.Bool{}
			u, inst, _ := newUpdater(t, r, tc.running, busy, tc.keys(r)...)
			if err := update(u, "1.1.0", true); err == nil {
				t.Fatal("update should fail")
			}
			if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
				t.Fatalf("state %v", s)
			}
			release, err := u.BeginCreate()
			if err != nil {
				t.Fatalf("create gated: %v", err)
			}
			release()
			if inst.installed.Load() != 0 {
				t.Fatal("installer ran")
			}
		})
	}
}

// TestTransferReservationExcludesInstallation checks that a pending verified
// update makes no installer or restart call while a snapshot transfer holds
// its reservation, installs once after the last release, and refuses new
// transfers with ErrInstalling after the installation started.
func TestTransferReservationExcludesInstallation(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	first, err := u.BeginTransfer()
	if err != nil {
		t.Fatal(err)
	}
	second, err := u.BeginTransfer()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("installed during transfers: %v %v", ok, err)
	}
	first()
	first() // A repeated release must not drop the reservation of the second.
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
		t.Fatal("installed while one transfer still runs")
	}
	// The wake channel holds one coalesced notification. The verified update
	// and the first release already queued one, so drain it: the receive
	// below must then prove that the last release queued a fresh one.
	select {
	case <-u.wake:
	default:
	}
	second()
	select {
	case <-u.wake:
	default:
		t.Fatal("the last release did not wake the installation loop")
	}
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("install after the release: %v %v", ok, err)
	}
	if _, err := u.BeginTransfer(); !errors.Is(err, ErrInstalling) {
		t.Fatalf("transfer during the installation: %v", err)
	}
}

// TestTransferReservationAfterFailedInstall checks that a failed installation
// opens the transfer gate again, and that a transfer and a live sandbox each
// defer the installation on their own.
func TestTransferReservationAfterFailedInstall(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	u, inst, _ := newUpdater(t, r, "1.0.0", busy)
	inst.err = errors.New("disk full")
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	busy.Store(true)
	end, err := u.BeginTransfer()
	if err != nil {
		t.Fatal(err)
	}
	end()
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
		t.Fatal("installed while a sandbox runs")
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); ok || err == nil || inst.installed.Load() != 1 {
		t.Fatalf("failing install: %v %v", ok, err)
	}
	end, err = u.BeginTransfer()
	if err != nil {
		t.Fatalf("transfer after the failed installation: %v", err)
	}
	end()
}

// gatedInstall holds Install so the test can observe an in-progress update.
// Closing done publishes the TryInstall result to the test and its cleanup.
type gatedInstall struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once

	installErr error
	installed  bool
	err        error
}

func startGatedInstall(t *testing.T, u *Updater, installErr error) *gatedInstall {
	t.Helper()
	g := &gatedInstall{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		done:       make(chan struct{}),
		installErr: installErr,
	}
	u.opts.Installer = g
	t.Cleanup(func() {
		g.open()
		g.awaitResult(t)
	})
	go func() {
		g.installed, g.err = u.TryInstall()
		close(g.done)
	}()
	return g
}

func (g *gatedInstall) Install(string) error {
	close(g.started)
	<-g.release
	return g.installErr
}

func (g *gatedInstall) open() {
	g.once.Do(func() { close(g.release) })
}

func (g *gatedInstall) awaitStarted() error {
	select {
	case <-g.started:
		return nil
	case <-g.done:
		if g.err != nil {
			return fmt.Errorf("TryInstall returned before installer started (installed=%t): %w", g.installed, g.err)
		}
		return fmt.Errorf("TryInstall returned before installer started (installed=%t, err=nil)", g.installed)
	case <-time.After(gateTimeout):
		return fmt.Errorf("installer did not start within %v", gateTimeout)
	}
}

func (g *gatedInstall) awaitResult(t *testing.T) (bool, error) {
	t.Helper()
	select {
	case <-g.done:
		return g.installed, g.err
	case <-time.After(gateTimeout):
		t.Fatalf("TryInstall did not finish within %v", gateTimeout)
		return false, nil
	}
}

func TestGatedInstallReportsEarlyStoreError(t *testing.T) {
	r := newRelease(t, nil)
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.opts.Store.Path(store.AgentFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := startGatedInstall(t, u, nil)
	err := g.awaitStarted()
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || !strings.Contains(err.Error(), "TryInstall returned before installer started (installed=false)") {
		t.Fatalf("want the early result with the store decode error, got %v", err)
	}
}

func TestGatedInstallReleasesOnCleanup(t *testing.T) {
	var g *gatedInstall
	if !t.Run("start installer", func(t *testing.T) {
		r := newRelease(t, nil)
		u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
		if err := update(u, "1.1.0", false); err != nil {
			t.Fatal(err)
		}
		g = startGatedInstall(t, u, nil)
		if err := g.awaitStarted(); err != nil {
			t.Fatal(err)
		}
	}) {
		return
	}
	if installed, err := g.awaitResult(t); !installed || err != nil {
		t.Fatalf("TryInstall after cleanup: installed=%t, err=%v", installed, err)
	}
}

// TestWithdrawDuringInstallLogsTheObservedTarget covers the late withdrawal:
// it returns ErrInstalling and logs the target that was installing.
func TestWithdrawDuringInstallLogsTheObservedTarget(t *testing.T) {
	r := newRelease(t, nil)
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	var logs strings.Builder
	u.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	inst := startGatedInstall(t, u, nil)
	if err := inst.awaitStarted(); err != nil {
		t.Fatal(err)
	}
	if err := u.Withdraw(); !errors.Is(err, ErrInstalling) {
		t.Fatalf("withdrawal during installation: %v", err)
	}
	inst.open()
	if installed, err := inst.awaitResult(t); !installed || err != nil {
		t.Fatalf("TryInstall: installed=%t, err=%v", installed, err)
	}
	if !strings.Contains(logs.String(), "agent update withdrawal too late") || !strings.Contains(logs.String(), "target_version=1.1.0") {
		t.Fatalf("late withdrawal log does not carry the target:\n%s", logs.String())
	}
}

// TestWithdrawRacesWithInstallFailure runs late withdrawals while a failing
// installation rewrites the target. Run with -race: the withdrawal must read
// the target that it logs while it holds the updater lock.
func TestWithdrawRacesWithInstallFailure(t *testing.T) {
	r := newRelease(t, nil)
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	u.opts.Logger = slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	installErr := errors.New("boom")
	for i := range 50 {
		if !t.Run(fmt.Sprint(i), func(t *testing.T) {
			u.mu.Lock()
			u.artifact = filepath.Join(t.TempDir(), "agent")
			u.target = "1.1.0"
			u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE
			u.mu.Unlock()
			inst := startGatedInstall(t, u, installErr)
			if err := inst.awaitStarted(); err != nil {
				t.Fatal(err)
			}
			// Withdrawals that run before the failure return ErrInstalling
			// and log the target.
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					for range 20 {
						u.WithdrawOrdered(u.NextOrder())
					}
				})
			}
			withdrawalsDone := make(chan struct{})
			go func() {
				wg.Wait()
				close(withdrawalsDone)
			}()
			t.Cleanup(func() {
				select {
				case <-withdrawalsDone:
				case <-time.After(gateTimeout):
					t.Fatalf("withdrawals did not finish within %v", gateTimeout)
				}
			})
			inst.open()
			if installed, err := inst.awaitResult(t); installed || !errors.Is(err, installErr) {
				t.Fatalf("TryInstall: installed=%t, err=%v", installed, err)
			}
		}) {
			return
		}
	}
}
