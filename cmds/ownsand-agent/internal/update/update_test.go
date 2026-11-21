// SPDX-License-Identifier: Apache-2.0

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
			if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
				t.Fatalf("state %v", s)
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
