package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type fixture struct {
	t        *testing.T
	key      ed25519.PrivateKey
	keys     []ReleaseKey
	srv      *httptest.Server
	manifest []byte
	artifact []byte
	mu       sync.Mutex
	busy     bool
	installs []string
	states   []ownsandv1.AgentUpdateState
}

func newFixture(t *testing.T) *fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fixture{t: t, key: priv, keys: []ReleaseKey{{ID: "k1", PublicKey: pub}}, artifact: []byte("new agent binary")}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write(f.manifest)
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.artifact) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) setManifest(m *Manifest, key ed25519.PrivateKey) {
	data, err := SignManifest(m, "k1", key)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.manifest = data
	f.mu.Unlock()
}

func (f *fixture) release(version string, def bool, hash []byte) Release {
	if hash == nil {
		sum := sha256.Sum256(f.artifact)
		hash = sum[:]
	}
	return Release{Version: version, Default: def, Artifacts: map[string]Artifact{"linux-amd64": {URL: f.srv.URL + "/agent", SHA256: hex.EncodeToString(hash)}}}
}

func (f *fixture) updater(current string) *Updater {
	return New(Config{
		CurrentVersion: current,
		Platform:       "linux-amd64",
		Keys:           f.keys,
		DownloadDir:    f.t.TempDir(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Busy: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.busy
		},
		Install: func(path string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.installs = append(f.installs, path)
			return nil
		},
		Report: func(s *ownsandv1.UpdateStatus) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.states = append(f.states, s.GetUpdateState())
		},
	})
}

func (f *fixture) run(u *Updater, target string, recovery bool) ownsandv1.AgentUpdateState {
	u.mu.Lock()
	u.target, u.recovery, u.manifestURL = target, recovery, f.srv.URL+"/manifest.json"
	u.mu.Unlock()
	u.Process(context.Background())
	s, _ := u.State()
	return s
}

func (f *fixture) installCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.installs)
}

func TestInstallsVerifiedUpdateWhenIdle(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}}, f.key)
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING {
		t.Fatalf("state %v", s)
	}
	if f.installCount() != 1 {
		t.Fatal("update not installed")
	}
}

func TestBadSignatureFails(t *testing.T) {
	f := newFixture(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}}, other)
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
	if f.installCount() != 0 {
		t.Fatal("installed with a bad signature")
	}
}

func TestBuiltinKeyListIsEmptyAndFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}}, f.key)
	if _, err := VerifyManifest(f.manifest, BuiltinReleaseKeys); err != ErrBadSignature {
		t.Fatalf("want ErrBadSignature with the built-in keys, got %v", err)
	}
}

func TestHashMismatchFails(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, make([]byte, 32))}}, f.key)
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED || f.installCount() != 0 {
		t.Fatalf("state %v, installs %d", s, f.installCount())
	}
}

func TestRevokedTargetFails(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}, Revoked: []string{"1.1.0"}}, f.key)
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
}

func TestUpdateWaitsWhileSandboxesRun(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}}, f.key)
	f.busy = true
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE {
		t.Fatalf("state %v", s)
	}
	// A create in progress also blocks the installation.
	done, err := u.BeginCreate()
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	u.Tick()
	if f.installCount() != 0 {
		t.Fatal("installed while a create was in progress")
	}
	done()
	u.Tick()
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING || f.installCount() != 1 {
		t.Fatalf("state %v, installs %d", s, f.installCount())
	}
	if _, err := u.BeginCreate(); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("create during INSTALLING: %v", err)
	}
}

func TestRevokedRunningVersionRejectsCreates(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.1.0", true, nil)}, Revoked: []string{"1.0.0"}}, f.key)
	f.busy = true
	u := f.updater("1.0.0")
	if s := f.run(u, "1.1.0", true); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
		t.Fatalf("state %v", s)
	}
	if _, err := u.BeginCreate(); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("create with a revoked version: %v", err)
	}
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	u.Tick()
	if f.installCount() != 1 {
		t.Fatal("revocation recovery not installed when idle")
	}
}

func TestDowngradeRules(t *testing.T) {
	f := newFixture(t)
	f.setManifest(&Manifest{Releases: []Release{f.release("1.0.0", true, nil), f.release("0.9.0", false, nil)}}, f.key)
	// A lower default without a revocation recovery is refused.
	u := f.updater("1.1.0")
	if s := f.run(u, "1.0.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("downgrade to the default: %v", s)
	}
	// A pinned lower version (not the default) is installed.
	u = f.updater("1.1.0")
	if s := f.run(u, "0.9.0", false); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING {
		t.Fatalf("pinned downgrade: %v", s)
	}
	// A revocation recovery installs a lower default.
	u = f.updater("1.1.0")
	if s := f.run(u, "1.0.0", true); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING {
		t.Fatalf("revocation recovery downgrade: %v", s)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.0.0", "1.0.1", -1}, {"1.10.0", "1.9.0", 1}, {"v2.0.0", "1.9.9", 1},
		{"1.0.0-rc1", "1.0.0", -1}, {"1.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
