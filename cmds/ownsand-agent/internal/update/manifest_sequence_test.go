// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func serveRelease(u *Updater, r *release) {
	u.opts.ManifestURL = r.srv.URL + "/manifest.json"
	u.opts.Keys = []ed25519.PublicKey{r.pub}
}

func serveManifestBytes(t *testing.T, u *Updater, key ed25519.PrivateKey, raw []byte) {
	t.Helper()
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw)))
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(raw) })
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u.opts.ManifestURL = srv.URL + "/manifest.json"
}

func TestOlderManifestCannotClearRevocation(t *testing.T) {
	current := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 2 })
	older := newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = nil })
	newer := newRelease(t, func(m *Manifest, _ *[]byte) {
		m.Sequence = 3
		m.RevokedVersions = nil
	})
	busy := &atomic.Bool{}
	busy.Store(true)
	u, _, _ := newUpdater(t, current, "0.9.0", busy)
	var logs bytes.Buffer
	u.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := update(u, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	revoked := ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
	requireUpdateState(t, u, revoked, "1.1.0")
	requireRevokedCreate(t, u)
	serveRelease(u, older)
	if err := update(u, "1.1.0", true); !errors.Is(err, ErrManifestRollback) {
		t.Fatalf("older update: %v", err)
	}
	requireUpdateState(t, u, revoked, "1.1.0")
	requireRevokedCreate(t, u)
	if got := older.artifactRequests.Load(); got != 0 {
		t.Fatalf("older artifact requests = %d", got)
	}
	for _, line := range [][]byte{
		[]byte(`level=INFO msg="release manifest accepted" manifest_sequence=2`),
		[]byte(`level=ERROR msg="release manifest rejected" manifest_sequence=1 accepted_sequence=2 reason=older_sequence`),
	} {
		if !bytes.Contains(logs.Bytes(), line) {
			t.Errorf("missing log %s in:\n%s", line, logs.Bytes())
		}
	}
	serveRelease(u, newer)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE, "1.1.0")
	done, err := u.BeginCreate()
	if err != nil {
		t.Fatalf("create after the newer manifest cleared revocation: %v", err)
	}
	done()
}

func TestOlderManifestCannotInstallRevokedTarget(t *testing.T) {
	current := newRelease(t, func(m *Manifest, _ *[]byte) {
		m.Sequence = 2
		m.RevokedVersions = []string{"1.1.0"}
	})
	older := newRelease(t, nil)
	u, inst, _ := newUpdater(t, current, "1.2.0", &atomic.Bool{})
	if err := update(u, "1.1.0", false); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked target: %v", err)
	}
	serveRelease(u, older)
	if err := update(u, "1.1.0", false); !errors.Is(err, ErrManifestRollback) {
		t.Fatalf("older update: %v", err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, "1.1.0")
	if installed, err := u.TryInstall(); installed || err != nil || inst.installed.Load() != 0 {
		t.Fatalf("unexpected install: installed=%v error=%v calls=%d", installed, err, inst.installed.Load())
	}
	if got := older.artifactRequests.Load(); got != 0 {
		t.Fatalf("older artifact requests = %d", got)
	}
}

func TestManifestSameSequenceConflict(t *testing.T) {
	r := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 3 })
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	var logs bytes.Buffer
	u.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	requireManifestCheckpoint(t, u, r, 3)
	checkpointPath := u.opts.Store.Path(store.AgentFile)
	stamp := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(checkpointPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	serveManifestBytes(t, u, r.priv, append(bytes.Clone(r.manifest), '\n'))
	if err := update(u, "1.1.0", false); !errors.Is(err, ErrManifestRollback) {
		t.Fatalf("conflicting content: %v", err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("manifest_sequence=3 accepted_sequence=3 reason=conflicting_content")) {
		t.Errorf("missing rejection in:\n%s", logs.Bytes())
	}
	if got := r.artifactRequests.Load(); got != 1 {
		t.Fatalf("artifact requests after conflict = %d, want 1", got)
	}
	serveRelease(u, r)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatalf("identical bytes: %v", err)
	}
	if info, err := os.Stat(checkpointPath); err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("checkpoint was rewritten: %v, %v", info, err)
	}
}

func TestManifestWithoutSequenceRejected(t *testing.T) {
	r := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 0 })
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	raw := bytes.Replace(r.manifest, []byte(`"sequence":0,`), nil, 1)
	serveManifestBytes(t, u, r.priv, raw)
	var logs bytes.Buffer
	u.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := update(u, "1.1.0", false); !errors.Is(err, ErrManifestNoSequence) {
		t.Fatalf("unsequenced update: %v", err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, "1.1.0")
	if !bytes.Contains(logs.Bytes(), []byte("manifest_sequence=0 accepted_sequence=0 reason=missing_sequence")) {
		t.Errorf("missing rejection in:\n%s", logs.Bytes())
	}
	if got := r.artifactRequests.Load(); got != 0 {
		t.Fatalf("artifact requests = %d", got)
	}
}

func TestManifestCheckpointSurvivesRestart(t *testing.T) {
	current := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 5 })
	older := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 4 })
	u, _, _ := newUpdater(t, current, "1.0.0", &atomic.Bool{})
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	opts := u.opts
	var err error
	opts.Store, err = store.Open(u.opts.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	serveRelease(restarted, older)
	if err := update(restarted, "1.1.0", false); !errors.Is(err, ErrManifestRollback) {
		t.Fatalf("older update after restart: %v", err)
	}
	requireManifestCheckpoint(t, restarted, current, 5)
}

func TestManifestCheckpointStoreFailure(t *testing.T) {
	current := newRelease(t, nil)
	newer := newRelease(t, func(m *Manifest, _ *[]byte) {
		m.Sequence = 2
		m.RevokedVersions = nil
	})
	u, _, _ := newUpdater(t, current, "0.9.0", &atomic.Bool{})
	if err := update(u, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	agentFile := u.opts.Store.Path(store.AgentFile)
	if err := os.Rename(agentFile, agentFile+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agentFile, 0o700); err != nil {
		t.Fatal(err)
	}
	serveRelease(u, newer)
	var pathErr *os.PathError
	if err := update(u, "1.1.0", true); !errors.As(err, &pathErr) {
		t.Fatalf("checkpoint store failure: %v", err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "1.1.0")
	requireRevokedCreate(t, u)
	if got := newer.artifactRequests.Load(); got != 0 {
		t.Fatalf("newer artifact requests after store failure = %d", got)
	}
	if err := os.Remove(agentFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(agentFile+".saved", agentFile); err != nil {
		t.Fatal(err)
	}
	requireManifestCheckpoint(t, u, current, 1)
}

func TestSupersededManifestAdvancesCheckpoint(t *testing.T) {
	r := newRelease(t, func(m *Manifest, _ *[]byte) { m.Sequence = 7 })
	u, _, _ := newUpdater(t, r, "0.9.0", &atomic.Bool{})
	gate := newGateTransport(t, r.srv.Client().Transport)
	gate.path = "/manifest.json.sig"
	u.opts.HTTP = &http.Client{Transport: gate}
	done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
	gate.awaitStartedOrEnd(t, done)
	if err := u.Withdraw(); err != nil {
		t.Fatal(err)
	}
	gate.open()
	if err := awaitDone(t, done); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("superseded update: %v", err)
	}
	requireManifestCheckpoint(t, u, r, 7)
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, "")
	createDone, err := u.BeginCreate()
	if err != nil {
		t.Fatalf("superseded manifest changed the create gate: %v", err)
	}
	createDone()
	if got := r.artifactRequests.Load(); got != 0 {
		t.Fatalf("superseded artifact requests = %d", got)
	}
}

func TestManifestSequenceIsAdditive(t *testing.T) {
	r := newRelease(t, nil)
	var old struct {
		Schema          int       `json:"schema"`
		Releases        []Release `json:"releases"`
		RevokedVersions []string  `json:"revoked_versions"`
	}
	if err := json.Unmarshal(r.manifest, &old); err != nil {
		t.Fatal(err)
	}
	if old.Schema != ManifestSchema || len(old.Releases) != 1 || len(old.RevokedVersions) != 1 {
		t.Fatalf("old manifest fields: %+v", old)
	}
}

func requireManifestCheckpoint(t *testing.T, u *Updater, r *release, sequence uint64) {
	t.Helper()
	state, err := u.opts.Store.LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(r.manifest)
	want := store.ManifestCheckpoint{Sequence: sequence, SHA256: hex.EncodeToString(sum[:])}
	if state.ReleaseManifest == nil || *state.ReleaseManifest != want {
		t.Fatalf("checkpoint = %+v, want %+v", state.ReleaseManifest, want)
	}
}
