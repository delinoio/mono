package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	// maxManifestSize bounds the manifest download.
	maxManifestSize = 1 << 20
	// maxArtifactSize bounds the artifact download.
	maxArtifactSize = 512 << 20
	// ManifestRefreshInterval is how often the agent fetches the manifest
	// again to learn about revocations.
	ManifestRefreshInterval = time.Hour
)

// Config configures an Updater.
type Config struct {
	CurrentVersion string
	// Platform is "<goos>-<goarch>".
	Platform string
	Keys     []ReleaseKey
	HTTP     *http.Client
	// DownloadDir holds the pending update artifact.
	DownloadDir string
	Logger      *slog.Logger
	// Busy reports whether a sandbox is creating, running or paused, or a
	// snapshot export or import runs.
	Busy func() bool
	// Install installs the verified artifact. On success the agent restarts.
	Install func(path string) error
	// Report receives every update state change. It must not block.
	Report func(*ownsandv1.UpdateStatus)
}

// Updater runs the update state machine. It also gates creates: once it
// reports INSTALLING or REVOKED_PENDING_IDLE, creates fail with unavailable,
// and it does not start the installation while a create is in progress.
type Updater struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	state       ownsandv1.AgentUpdateState
	target      string
	recovery    bool
	manifestURL string
	revoked     bool
	pending     string // verified artifact path
	pendingFor  string // version of the pending artifact
	creates     int
	message     string
	lastFetch   time.Time
	working     bool
}

// New returns an Updater in state UP_TO_DATE.
func New(cfg Config) *Updater {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if cfg.Report == nil {
		cfg.Report = func(*ownsandv1.UpdateStatus) {}
	}
	if cfg.Busy == nil {
		cfg.Busy = func() bool { return false }
	}
	return &Updater{cfg: cfg, log: cfg.Logger, state: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE}
}

// State returns the update state and the target version.
func (u *Updater) State() (ownsandv1.AgentUpdateState, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state, u.target
}

// Status returns the current update status message.
func (u *Updater) Status() *ownsandv1.UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.statusLocked()
}

func (u *Updater) statusLocked() *ownsandv1.UpdateStatus {
	return &ownsandv1.UpdateStatus{AgentVersion: u.cfg.CurrentVersion, UpdateState: u.state, TargetVersion: u.target, Message: u.message}
}

func (u *Updater) setLocked(s ownsandv1.AgentUpdateState, msg string) {
	if u.state == s && u.message == msg {
		return
	}
	u.log.Info("agent update state changed", "from", u.state.String(), "to", s.String(), "target_version", u.target, "message", msg)
	u.state, u.message = s, msg
	u.cfg.Report(u.statusLocked())
}

// BeginCreate admits a create unless the update installs or the running
// version is revoked.
func (u *Updater) BeginCreate() (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.state == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent is installing an update"))
	case u.revoked:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the running agent version is revoked"))
	}
	u.creates++
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			u.creates--
			u.mu.Unlock()
		})
	}, nil
}

// SetTarget sets the target version from the configuration or an update
// command and starts the verification in the background.
func (u *Updater) SetTarget(ctx context.Context, target string, recovery bool, manifestURL string) {
	u.mu.Lock()
	if manifestURL != "" {
		u.manifestURL = manifestURL
	}
	changed := target != u.target || recovery != u.recovery
	u.target, u.recovery = target, recovery
	start := changed && !u.working
	if start {
		u.working = true
	}
	u.mu.Unlock()
	if start {
		go func() {
			u.process(ctx)
			u.mu.Lock()
			u.working = false
			u.mu.Unlock()
		}()
	}
}

// Process runs one verification and download pass synchronously. Tests call
// it directly.
func (u *Updater) Process(ctx context.Context) { u.process(ctx) }

func (u *Updater) process(ctx context.Context) {
	u.mu.Lock()
	target, recovery, url := u.target, u.recovery, u.manifestURL
	u.mu.Unlock()
	if url == "" {
		if target != "" && target != u.cfg.CurrentVersion {
			u.fail("no release manifest URL is configured")
		}
		return
	}
	m, err := u.fetchManifest(ctx, url)
	if err != nil {
		u.fail(err.Error())
		return
	}
	u.mu.Lock()
	u.lastFetch = time.Now()
	u.revoked = m.IsRevoked(u.cfg.CurrentVersion)
	u.mu.Unlock()
	if target == "" || target == u.cfg.CurrentVersion {
		u.mu.Lock()
		if u.revoked {
			u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "the running version is revoked")
		} else {
			u.pending = ""
			u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, "")
		}
		u.mu.Unlock()
		return
	}
	if m.IsRevoked(target) {
		u.fail("the target version is revoked")
		return
	}
	// A lower version is installed only for a pin or a revocation recovery.
	// The control plane sends the manifest default under FOLLOW_DEFAULT, so
	// a lower target that is the default (for example, after the default
	// moved back) is refused, and a lower target that is not the default is
	// a pin of the organization policy.
	if CompareVersions(target, u.cfg.CurrentVersion) < 0 && !recovery && target == m.DefaultVersion() {
		u.fail("refusing to downgrade to the default version without a pin or a revocation recovery")
		return
	}
	rel, ok := m.Release(target)
	if !ok {
		u.fail("the target version is not in the release manifest")
		return
	}
	art, sum, err := rel.ArtifactFor(u.cfg.Platform)
	if err != nil {
		u.fail(err.Error())
		return
	}
	u.mu.Lock()
	u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, "")
	u.mu.Unlock()
	path, err := u.download(ctx, art.URL, sum, target)
	if err != nil {
		u.fail(err.Error())
		return
	}
	u.mu.Lock()
	u.pending, u.pendingFor = path, target
	u.idleStateLocked()
	u.mu.Unlock()
	u.Tick()
}

func (u *Updater) idleStateLocked() {
	if u.revoked {
		u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "waiting for the sandboxes to end")
	} else {
		u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE, "waiting for the sandboxes to end")
	}
}

func (u *Updater) fail(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pending = ""
	u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, msg)
}

// Tick starts the installation of a verified update when the device is idle
// and no create is in progress, and refreshes the manifest periodically.
func (u *Updater) Tick() {
	u.mu.Lock()
	if u.pending == "" {
		refresh := u.manifestURL != "" && !u.working && time.Since(u.lastFetch) >= ManifestRefreshInterval
		u.mu.Unlock()
		if refresh {
			u.mu.Lock()
			u.working = true
			u.mu.Unlock()
			go func() {
				u.process(context.Background())
				u.mu.Lock()
				u.working = false
				u.mu.Unlock()
			}()
		}
		return
	}
	if u.creates > 0 || u.cfg.Busy() {
		u.idleStateLocked()
		u.mu.Unlock()
		return
	}
	path, version := u.pending, u.pendingFor
	u.setLocked(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING, "")
	u.mu.Unlock()
	u.log.Info("installing agent update", "version", version)
	if err := u.cfg.Install(path); err != nil {
		u.log.Error("agent update installation failed", "version", version, "error", err)
		u.fail("installation failed")
	}
}

func (u *Updater) fetchManifest(ctx context.Context, url string) (*Manifest, error) {
	data, err := u.get(ctx, url, maxManifestSize)
	if err != nil {
		return nil, fmt.Errorf("download release manifest: %w", err)
	}
	m, err := VerifyManifest(data, u.cfg.Keys)
	if err != nil {
		u.log.Error("release manifest verification failed", "error", err)
		return nil, err
	}
	return m, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := u.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download exceeds the size limit")
	}
	return data, nil
}

// download fetches the artifact and verifies its SHA-256 before it keeps it.
func (u *Updater) download(ctx context.Context, url string, want []byte, version string) (string, error) {
	data, err := u.get(ctx, url, maxArtifactSize)
	if err != nil {
		return "", fmt.Errorf("download release artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], want) {
		u.log.Error("release artifact hash mismatch", "version", version)
		return "", errors.New("release artifact hash mismatch")
	}
	if err := os.MkdirAll(u.cfg.DownloadDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(u.cfg.DownloadDir, "ownsand-agent-"+version)
	if err := os.WriteFile(path, data, 0o700); err != nil {
		return "", err
	}
	u.log.Info("release artifact verified", "version", version, "size_bytes", len(data))
	return path, nil
}
