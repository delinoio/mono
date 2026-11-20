// Package sandbox owns the agent's sandbox and snapshot records: the command
// handling of each sandbox in order, the device limits check, TTL
// enforcement (also after a restart), revisions and their acknowledgement,
// lost VMs and the guest connection of each sandbox.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestconn"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// IdleReportInterval is the shortest time between two sandbox updates
	// that only move the idle deadline.
	IdleReportInterval = time.Minute
	// MarkerRetention is how long a delete marker rejects a late create.
	MarkerRetention = 24 * time.Hour
	// GuestReadyTimeout bounds the wait for the guest helper after boot.
	GuestReadyTimeout = 2 * time.Minute
	// MaxMacOSGuests is the Virtualization.framework limit of macOS guests.
	MaxMacOSGuests = 2
)

// Reporter receives sandbox and snapshot updates for the agent channel. Its
// methods must not block.
type Reporter interface {
	SandboxUpdate(*ownsandv1.SandboxReport)
	SnapshotUpdate(*ownsandv1.SnapshotUpdate)
}

// CreateGate makes creates and the update installation mutually exclusive.
type CreateGate interface {
	// BeginCreate fails with unavailable while an update installs or while
	// the running version is revoked.
	BeginCreate() (done func(), err error)
}

type openGate struct{}

func (openGate) BeginCreate() (func(), error) { return func() {}, nil }

type discardReporter struct{}

func (discardReporter) SandboxUpdate(*ownsandv1.SandboxReport)   {}
func (discardReporter) SnapshotUpdate(*ownsandv1.SnapshotUpdate) {}

// Config configures a Manager.
type Config struct {
	Runtime  runtime.Runtime
	Dir      *state.Dir
	Logger   *slog.Logger
	Now      func() time.Time
	Reporter Reporter
	Gate     CreateGate
	// FreeDisk returns the free disk of the state directory in bytes.
	FreeDisk func() int64
	// GuestReadyTimeout overrides GuestReadyTimeout.
	GuestReadyTimeout time.Duration
}

// Manager owns the sandboxes of the device.
type Manager struct {
	cfg Config
	log *slog.Logger
	ctx context.Context

	mu             sync.Mutex
	recs           *state.Records
	fresh          bool
	limits         *ownsandv1.DeviceLimits
	creates        map[string]context.CancelFunc
	expiring       map[string]bool
	active         map[string]int
	lastIdleReport map[string]time.Time
	pending        map[string]*ownsandv1.SandboxReport
	reportID       uint64
	reportContents map[uint64]map[string]uint64

	qmu    sync.Mutex
	queues map[string]*queue

	gmu    sync.Mutex
	guests map[string]*guestconn.Client
}

type queue struct {
	jobs    []func()
	running bool
}

// New loads the records and returns a Manager. Call Recover before serving.
func New(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Reporter == nil {
		cfg.Reporter = discardReporter{}
	}
	if cfg.Gate == nil {
		cfg.Gate = openGate{}
	}
	if cfg.FreeDisk == nil {
		cfg.FreeDisk = func() int64 { return 1 << 62 }
	}
	if cfg.GuestReadyTimeout <= 0 {
		cfg.GuestReadyTimeout = GuestReadyTimeout
	}
	recs, fresh, err := cfg.Dir.LoadRecords()
	if err != nil {
		return nil, fmt.Errorf("load records: %w", err)
	}
	return &Manager{
		cfg:            cfg,
		log:            cfg.Logger,
		ctx:            ctx,
		recs:           recs,
		fresh:          fresh,
		creates:        map[string]context.CancelFunc{},
		expiring:       map[string]bool{},
		active:         map[string]int{},
		lastIdleReport: map[string]time.Time{},
		pending:        map[string]*ownsandv1.SandboxReport{},
		reportContents: map[uint64]map[string]uint64{},
		queues:         map[string]*queue{},
		guests:         map[string]*guestconn.Client{},
	}, nil
}

// Epoch returns the state epoch that the agent sends with Authenticate.
func (m *Manager) Epoch() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recs.Epoch
}

// SetLimits stores the device limits of the last configuration message.
func (m *Manager) SetLimits(l *ownsandv1.DeviceLimits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits = l
}

func (m *Manager) saveLocked() {
	if err := m.cfg.Dir.SaveRecords(m.recs); err != nil {
		m.log.Error("save records failed", "error", err)
	}
}

func (m *Manager) revisionLocked(r *state.SandboxRecord) *ownsandv1.SandboxRevision {
	return &ownsandv1.SandboxRevision{Epoch: m.recs.Epoch, Counter: r.Counter}
}

func (m *Manager) reportLocked(r *state.SandboxRecord) *ownsandv1.SandboxReport {
	rep := &ownsandv1.SandboxReport{
		SandboxId:   r.SandboxID,
		State:       r.State,
		EndReason:   r.EndReason,
		Revision:    m.revisionLocked(r),
		RestorePath: r.RestorePath,
	}
	if !r.IdleDeadline.IsZero() {
		rep.IdleDeadline = timestamppb.New(r.IdleDeadline)
	}
	return rep
}

// changeLocked applies a local change: it increases the revision, saves the
// records and queues a sandbox update that is resent until acknowledged.
func (m *Manager) changeLocked(r *state.SandboxRecord, change func(r *state.SandboxRecord)) {
	prev := r.State
	change(r)
	r.Counter++
	m.recs.Sandboxes[r.SandboxID] = r
	m.saveLocked()
	rep := m.reportLocked(r)
	m.pending[r.SandboxID] = rep
	if prev != r.State {
		m.log.Info("sandbox state changed", "sandbox_id", r.SandboxID, "from", prev.String(), "to", r.State.String(),
			"end_reason", r.EndReason.String(), "revision", r.Counter)
	}
	m.cfg.Reporter.SandboxUpdate(rep)
}

// PendingUpdates returns the sandbox updates that are not acknowledged yet.
func (m *Manager) PendingUpdates() []*ownsandv1.SandboxReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Values(m.pending))
}

// StateReport builds a full state report with a new report ID.
func (m *Manager) StateReport() *ownsandv1.StateReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reportID++
	rep := &ownsandv1.StateReport{ReportId: m.reportID}
	terminal := map[string]uint64{}
	for _, id := range slices.Sorted(maps.Keys(m.recs.Sandboxes)) {
		r := m.recs.Sandboxes[id]
		rep.Sandboxes = append(rep.Sandboxes, m.reportLocked(r))
		if r.Terminal() {
			terminal[id] = r.Counter
		}
	}
	m.reportContents[m.reportID] = terminal
	for _, id := range slices.Sorted(maps.Keys(m.recs.Snapshots)) {
		rep.Snapshots = append(rep.Snapshots, snapshotReport(m.recs.Snapshots[id]))
	}
	return rep
}

func snapshotReport(s *state.SnapshotRecord) *ownsandv1.SnapshotReport {
	return &ownsandv1.SnapshotReport{SnapshotId: s.SnapshotID, State: s.State, SizeBytes: s.SizeBytes, HasMemoryState: s.HasMemoryState}
}

// AckReport handles the acknowledgement of a state report: the agent drops
// the records of the ended sandboxes that the report contained.
func (m *Manager) AckReport(reportID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	contents, ok := m.reportContents[reportID]
	if !ok {
		return
	}
	for id := range m.reportContents {
		if id <= reportID {
			delete(m.reportContents, id)
		}
	}
	changed := false
	for id, counter := range contents {
		r := m.recs.Sandboxes[id]
		if r == nil || r.Counter != counter {
			continue
		}
		r.AckedCounter = max(r.AckedCounter, counter)
		if p := m.pending[id]; p != nil && p.GetRevision().GetCounter() <= counter {
			delete(m.pending, id)
		}
		changed = m.dropAckedLocked(r) || changed
	}
	if changed {
		m.saveLocked()
	}
}

// AckSandbox handles the acknowledgement of a sandbox update.
func (m *Manager) AckSandbox(sandboxID string, rev *ownsandv1.SandboxRevision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rev.GetEpoch() != m.recs.Epoch {
		return
	}
	if p := m.pending[sandboxID]; p != nil && p.GetRevision().GetCounter() <= rev.GetCounter() {
		delete(m.pending, sandboxID)
	}
	r := m.recs.Sandboxes[sandboxID]
	if r == nil {
		return
	}
	r.AckedCounter = max(r.AckedCounter, rev.GetCounter())
	m.dropAckedLocked(r)
	m.saveLocked()
}

// dropAckedLocked drops an ended sandbox record whose terminal revision is
// acknowledged. Delete markers stay until MarkerRetention ends.
func (m *Manager) dropAckedLocked(r *state.SandboxRecord) bool {
	if !r.Terminal() || r.DeleteMarker || r.AckedCounter < r.Counter {
		return false
	}
	delete(m.recs.Sandboxes, r.SandboxID)
	delete(m.pending, r.SandboxID)
	m.log.Info("sandbox record dropped after acknowledgement", "sandbox_id", r.SandboxID)
	return true
}

// enqueue runs job after the earlier jobs of the same sandbox.
func (m *Manager) enqueue(id string, job func()) {
	m.qmu.Lock()
	defer m.qmu.Unlock()
	q := m.queues[id]
	if q == nil {
		q = &queue{}
		m.queues[id] = q
	}
	q.jobs = append(q.jobs, job)
	if !q.running {
		q.running = true
		go m.drain(id, q)
	}
}

func (m *Manager) drain(id string, q *queue) {
	for {
		m.qmu.Lock()
		if len(q.jobs) == 0 {
			q.running = false
			delete(m.queues, id)
			m.qmu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs = q.jobs[1:]
		m.qmu.Unlock()
		job()
	}
}

// Result builds a command result. A nil error is a success.
func Result(commandID string, err error) *ownsandv1.AgentCommandResult {
	res := &ownsandv1.AgentCommandResult{CommandId: commandID}
	if err != nil {
		code := connect.CodeOf(err)
		if code == connect.CodeUnknown {
			code = connect.CodeInternal
		}
		res.ErrorCode = code.String()
		var ce *connect.Error
		if errors.As(err, &ce) {
			res.ErrorMessage = ce.Message()
		} else {
			res.ErrorMessage = err.Error()
		}
	}
	return res
}

// Handle handles a sandbox or snapshot command. It returns false for a
// command that the manager does not own. reply is called once, possibly from
// another goroutine.
func (m *Manager) Handle(cmd *ownsandv1.AgentCommand, reply func(*ownsandv1.AgentCommandResult)) bool {
	id := cmd.GetCommandId()
	switch c := cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CreateSandbox:
		sid := c.CreateSandbox.GetSandboxId()
		m.enqueue(sid, func() { reply(m.create(id, c.CreateSandbox)) })
	case *ownsandv1.AgentCommand_DeleteSandbox:
		sid := c.DeleteSandbox.GetSandboxId()
		// A delete cancels a create that is still running, before it waits
		// for its turn in the sandbox's queue.
		m.mu.Lock()
		if cancel := m.creates[sid]; cancel != nil {
			m.log.Info("delete cancels a running create", "sandbox_id", sid, "command_id", id)
			cancel()
		}
		m.mu.Unlock()
		reason := c.DeleteSandbox.GetReason()
		if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
			reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
		}
		m.enqueue(sid, func() { reply(m.withReport(id, sid, m.delete(sid, reason))) })
	case *ownsandv1.AgentCommand_PauseSandbox:
		sid := c.PauseSandbox.GetSandboxId()
		m.enqueue(sid, func() { reply(m.withReport(id, sid, m.pause(sid))) })
	case *ownsandv1.AgentCommand_ResumeSandbox:
		sid := c.ResumeSandbox.GetSandboxId()
		m.enqueue(sid, func() { reply(m.withReport(id, sid, m.resume(sid))) })
	case *ownsandv1.AgentCommand_CreateSnapshot:
		sid := c.CreateSnapshot.GetSandboxId()
		m.enqueue(sid, func() { reply(m.snapshot(id, c.CreateSnapshot)) })
	case *ownsandv1.AgentCommand_DeleteSnapshot:
		go func() { reply(Result(id, m.deleteSnapshot(c.DeleteSnapshot.GetSnapshotId()))) }()
	case *ownsandv1.AgentCommand_ExportSnapshot, *ownsandv1.AgentCommand_ImportSnapshot:
		// Snapshot export and import through R2 signed URLs are not
		// implemented in this agent version.
		reply(Result(id, connect.NewError(connect.CodeUnimplemented, errors.New("snapshot export and import are not implemented by this agent"))))
	default:
		return false
	}
	return true
}

func (m *Manager) withReport(commandID, sandboxID string, err error) *ownsandv1.AgentCommandResult {
	res := Result(commandID, err)
	if err == nil {
		m.mu.Lock()
		if r := m.recs.Sandboxes[sandboxID]; r != nil {
			res.Result = &ownsandv1.AgentCommandResult_Sandbox{Sandbox: m.reportLocked(r)}
		}
		m.mu.Unlock()
	}
	return res
}

// Report returns the current report of a sandbox, or nil.
func (m *Manager) Report(sandboxID string) *ownsandv1.SandboxReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.recs.Sandboxes[sandboxID]; r != nil {
		return m.reportLocked(r)
	}
	return nil
}

// SandboxState returns the state of a sandbox, or UNSPECIFIED when unknown.
func (m *Manager) SandboxState(sandboxID string) ownsandv1.SandboxState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.recs.Sandboxes[sandboxID]; r != nil && !r.DeleteMarker {
		return r.State
	}
	return ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
}

// Busy reports whether a sandbox is creating, running or paused, which
// blocks an update installation.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.creates) > 0 {
		return true
	}
	for _, r := range m.recs.Sandboxes {
		switch r.State {
		case ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
			return true
		}
	}
	return false
}

// Counts returns the number of sandboxes per state, for `status`.
func (m *Manager) Counts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, r := range m.recs.Sandboxes {
		if !r.DeleteMarker {
			out[r.State.String()]++
		}
	}
	return out
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(what+" is unknown on this device"))
}

func failedPrecondition(msg string) error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}
