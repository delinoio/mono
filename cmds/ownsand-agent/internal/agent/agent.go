// SPDX-License-Identifier: FSL-1.1-ALv2

// Package agent wires the device agent together: the agent channel with
// authentication, reconnect back-off, heartbeats, capability and state
// reports, the command handling through the sandbox manager, the update
// flow, snapshot transfers, device removal and the data path component.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/snapxfer"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/internal/backoff"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timing rules of the agent channel contract.
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultAckTimeout        = 45 * time.Second
	// CapabilityInterval: the free disk is reported at least every 60
	// seconds, and when it changes by more than 1 GiB.
	CapabilityInterval  = 60 * time.Second
	capabilityDiskDelta = 1 << 30
)

// ErrRemoved reports a finished device removal: the identity is deleted
// and the agent is no longer enrolled.
var ErrRemoved = errors.New("agent: the device was removed and its local data deleted")

// ErrRestart reports that the agent installed an update and must restart.
var ErrRestart = errors.New("agent: restart to run the installed update")

// Options configure an Agent.
type Options struct {
	Store    *store.Store
	Identity *store.Identity
	Control  *cpclient.Client
	Runtime  vmruntime.Runtime
	Logger   *slog.Logger
	// DataPath defaults to NoDataPath. NewDataPath, when set, builds it
	// from the agent (the data path needs the dispatcher).
	NewDataPath func(a *Agent) DataPath
	// Update configures the update flow (manifest URL, keys, installer).
	Update update.Options
	// Heartbeat and acknowledgement timing; zero values use the contract
	// defaults.
	HeartbeatInterval time.Duration
	AckTimeout        time.Duration
	// BackoffMin and BackoffMax bound the reconnect delays.
	BackoffMin, BackoffMax time.Duration
}

// Agent is the running device agent.
type Agent struct {
	opts      Options
	logger    *slog.Logger
	key       *identity.Key
	st        *store.Store
	cp        *cpclient.Client
	rt        vmruntime.Runtime
	Sandboxes *sandboxes.Manager
	Ops       *ops.Dispatcher
	updater   *update.Updater
	transfer  *snapxfer.Transfer
	dataPath  DataPath

	mu         sync.Mutex
	conn       *conn
	generation uint64
	reportSeq  uint64
	urlWaiters map[string]chan *ownsandv1.SignedUrlGrant
	restart    chan struct{}
	removal    chan struct{}
	status     Status
}

// New builds the agent and reconciles its local state.
func New(ctx context.Context, opts Options) (*Agent, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.AckTimeout <= 0 {
		opts.AckTimeout = DefaultAckTimeout
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Second
	}
	key, err := identity.FromSeed(opts.Identity.PrivateKeySeed)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		opts: opts, logger: opts.Logger, key: key, st: opts.Store, cp: opts.Control, rt: opts.Runtime,
		urlWaiters: map[string]chan *ownsandv1.SignedUrlGrant{},
		restart:    make(chan struct{}, 1),
		removal:    make(chan struct{}, 1),
	}
	a.Sandboxes, err = sandboxes.New(ctx, sandboxes.Options{Store: opts.Store, Runtime: opts.Runtime, Logger: opts.Logger})
	if err != nil {
		return nil, err
	}
	a.Ops, err = ops.New(a.Sandboxes, opts.Store, ops.Options{Logger: opts.Logger})
	if err != nil {
		return nil, err
	}
	a.Sandboxes.SetExecTracker(a.Ops)
	a.Sandboxes.SetOutbox(a)
	uopts := opts.Update
	uopts.Store = opts.Store
	uopts.Logger = opts.Logger
	uopts.Busy = a.Sandboxes.Busy
	uopts.Restart = func() {
		select {
		case a.restart <- struct{}{}:
		default:
		}
	}
	a.updater, err = update.New(uopts)
	if err != nil {
		return nil, err
	}
	a.Sandboxes.SetCreateGate(updateGate{a.updater})
	httpClient := &http.Client{Timeout: 30 * time.Minute}
	a.transfer = &snapxfer.Transfer{HTTP: httpClient, URLs: a, Logger: opts.Logger, TempDir: opts.Store.Path(store.SnapshotsDir)}
	if opts.NewDataPath != nil {
		a.dataPath = opts.NewDataPath(a)
	} else {
		a.dataPath = NoDataPath{}
	}
	if st, err := opts.Store.LoadAgent(); err == nil && len(st.STUNURLs) > 0 {
		a.dataPath.Configure(st.STUNURLs)
	}
	return a, nil
}

// updateGate maps update errors to command errors.
type updateGate struct{ u *update.Updater }

func (g updateGate) BeginCreate() (func(), error) {
	release, err := g.u.BeginCreate()
	if err != nil {
		return nil, &sandboxes.CommandError{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: err.Error()}
	}
	return release, nil
}

// Identity returns the device ID.
func (a *Agent) DeviceID() string { return a.opts.Identity.DeviceID }

// Key returns the device key.
func (a *Agent) Key() *identity.Key { return a.key }

// Control returns the control plane client.
func (a *Agent) Control() *cpclient.Client { return a.cp }

// NewDeviceSession gets a new device session credential with
// CreateChallenge and Authenticate, for one relay connection. Secret: the
// caller must not log it.
func (a *Agent) NewDeviceSession(ctx context.Context) (string, error) {
	st, err := a.st.LoadAgent()
	if err != nil {
		return "", err
	}
	res, err := a.cp.Authenticate(ctx, a.DeviceID(), a.key, st.StateEpoch)
	if err != nil {
		return "", err
	}
	if res.Removal != nil {
		select {
		case a.removal <- struct{}{}:
		default:
		}
		return "", errors.New("agent: the device was removed")
	}
	return res.Session.Credential, nil
}

// Logger returns the agent logger.
func (a *Agent) Logger() *slog.Logger { return a.logger }

// Run runs the agent until ctx ends, the device is removed (ErrRemoved) or
// an update was installed (ErrRestart).
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.shutdown()
	if st, err := a.st.LoadAgent(); err == nil && st.Removal.NoticeReceived {
		a.logger.Info("continuing an interrupted device removal")
		return a.removeDevice(ctx)
	}
	var wg sync.WaitGroup
	for _, f := range []func(context.Context){a.Sandboxes.Run, a.Ops.Run, a.updater.Run, a.statusLoop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}
	defer func() {
		cancel()
		wg.Wait()
	}()
	chCtx, chCancel := context.WithCancel(ctx)
	defer chCancel()
	errc := make(chan error, 1)
	go func() { errc <- a.channelLoop(chCtx) }()
	select {
	case err := <-errc:
		cancel()
		return err
	case <-a.restart:
		cancel()
		<-errc
		return ErrRestart
	case <-a.removal:
		// The control plane closes the channel after the clean-up command.
		chCancel()
		<-errc
		return a.removeDevice(ctx)
	case <-ctx.Done():
		<-errc
		return nil
	}
}

func (a *Agent) shutdown() {
	a.Sandboxes.Close()
	a.dataPath.Close()
	a.Ops.Close()
	if err := a.rt.Close(); err != nil {
		a.logger.Warn("runtime close failed", "error", err)
	}
}

// channelLoop keeps one agent channel open, with back-off between
// attempts.
func (a *Agent) channelLoop(ctx context.Context) error {
	b := backoff.Backoff{Min: a.opts.BackoffMin, Max: a.opts.BackoffMax}
	for {
		started := time.Now()
		err := a.runChannel(ctx)
		if errors.Is(err, ErrRemoved) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if time.Since(started) > time.Minute {
			b.Reset()
		}
		delay := b.Next()
		a.logger.Warn("agent channel closed; reconnecting", "error", errString(err), "retry_in_ms", delay.Milliseconds())
		if backoff.Sleep(ctx, delay) != nil {
			return nil
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SendSandboxUpdate implements sandboxes.Outbox.
func (a *Agent) SendSandboxUpdate(e *ownsandv1.SandboxStateEntry) bool {
	return a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: e}}})
}

// send queues an envelope on the current channel.
func (a *Agent) send(env *ownsandv1.AgentEnvelope) bool {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return false
	}
	return c.Send(env)
}

// RequestURLs implements snapxfer.URLSource over the agent channel.
func (a *Agent) RequestURLs(ctx context.Context, snapshotID string, op ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error) {
	requestID := fmt.Sprintf("%s-%d-%d", snapshotID, time.Now().UnixNano(), len(parts))
	ch := make(chan *ownsandv1.SignedUrlGrant, 1)
	a.mu.Lock()
	a.urlWaiters[requestID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.urlWaiters, requestID)
		a.mu.Unlock()
	}()
	if !a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{
		RequestId: requestID, SnapshotId: snapshotID, Operation: op, PartNumbers: parts,
	}}}) {
		return nil, errors.New("agent: no agent channel for signed URLs")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, errors.New("agent: no signed URL grant within a minute")
	case g := <-ch:
		if g.GetError() != nil {
			return nil, fmt.Errorf("agent: signed URL request rejected: %s", g.GetError().GetMessage())
		}
		return g.GetPartUrls(), nil
	}
}

func (a *Agent) deliverURLs(g *ownsandv1.SignedUrlGrant) {
	a.mu.Lock()
	ch := a.urlWaiters[g.GetRequestId()]
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- g:
		default:
		}
	}
}
