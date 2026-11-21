// SPDX-License-Identifier: Apache-2.0

// Package ops serves the DataFrame operations of the data path (exec, exec
// attach, file read and file write) for both transports, the peer-to-peer
// data channel and the relay connection. It owns the exec bookkeeping
// (at-most-once exec_id, kept output, 10-minute retention that survives
// agent restarts for the at-most-once rule), the admission window and the
// 30-minute memory of operation IDs, the recorded request digests of each
// operation and the relay grant checks of the first request frame.
//
// Requests, output and file data are sandbox content: nothing here logs
// them.
package ops

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-guest/execlog"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Retention of the file operation records on the agent: the guest helper
// keeps a read snapshot or a write for 15 minutes after its last use.
const fileOpRetention = 15 * time.Minute

// persistedExecRetention: an exec_id older than 15 minutes (plus 5 minutes
// of tolerated clock skew) fails the admission window, so a persisted
// exec record is needed only until 20 minutes after its exec_id time.
const persistedExecRetention = datapath.OperationIDMaxAge + datapath.OperationIDMaxFuture

// KeepaliveInterval is how often an exec stream without output gets a
// keepalive event; the contract requires at least one every 60 seconds.
const KeepaliveInterval = 30 * time.Second

// Sandboxes gives the dispatcher the state and guest connection of each
// sandbox.
type Sandboxes interface {
	// State returns the current state of a sandbox known to the agent.
	State(sandboxID string) (ownsandv1.SandboxState, bool)
	// Guest returns the guest helper client of a running or paused sandbox.
	Guest(sandboxID string) (*guestclient.Client, bool)
	// Touch records sandbox activity, which restarts the idle clock.
	Touch(sandboxID string)
}

// Sink sends frames toward the caller. Send blocks while the transport's
// credit window is exhausted and fails when the stream ended.
type Sink interface {
	Send(ctx context.Context, f *ownsandv1.DataFrame) error
}

// GrantCheck is the relay grant of a relay stream.
type GrantCheck struct {
	Kind        ownsandv1.RelayOperationKind
	OperationID string
	// Digest is absent for ATTACH_EXEC grants.
	Digest []byte
}

// Request is the first request frame of a stream.
type Request struct {
	Frame *ownsandv1.DataFrame
	// SandboxID is the sandbox that the session or the grant authorized.
	SandboxID string
	// Grant is set on relay streams.
	Grant *GrantCheck
	// OnAccepted runs when the agent accepts an Exec, AttachExec or
	// ReadFile request, before any output; the relay sends its request
	// accepted notice from it.
	OnAccepted func()
}

// Outcome reports how a request ended, for grant release decisions.
type Outcome struct {
	// Finished: the operation finished on the agent and its result was
	// delivered on this stream (exec result, final read message, write
	// finish result).
	Finished bool
	// Terminal: the request failed with an error that leaves no operation
	// to continue.
	Terminal bool
	// Error is the error frame that was sent, if any.
	Error *ownsandv1.ErrorDetail
}

type execOp struct {
	sandboxID string
	digest    []byte
	idTime    time.Time
	log       *execlog.Log
	cancel    context.CancelFunc
}

type fileOp struct {
	sandboxID string
	digest    []byte
	lastUse   time.Time
	open      int
}

// persistedExec is the record of one accepted exec_id in execs.json.
type persistedExec struct {
	ExecID    string    `json:"exec_id"`
	SandboxID string    `json:"sandbox_id"`
	Digest    []byte    `json:"digest"`
	IDTime    time.Time `json:"id_time"`
}

// Dispatcher serves data path operations. It is safe for concurrent use.
type Dispatcher struct {
	sb     Sandboxes
	st     *store.Store
	logger *slog.Logger
	now    func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// execs are the execs with an in-memory log.
	execs map[string]*execOp
	// lost are exec_ids accepted by an earlier agent process; their
	// output is gone, but they never run again.
	lost   map[string]persistedExec
	reads  map[string]*fileOp
	writes map[string]*fileOp
	// ended remembers ended or expired operation IDs for 30 minutes.
	ended map[string]time.Time
}

// Options configure a Dispatcher.
type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns a dispatcher and loads the exec records of an earlier agent
// process.
func New(sb Sandboxes, st *store.Store, opts Options) (*Dispatcher, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		sb: sb, st: st, logger: opts.Logger, now: opts.Now,
		ctx: ctx, cancel: cancel,
		execs: map[string]*execOp{}, lost: map[string]persistedExec{},
		reads: map[string]*fileOp{}, writes: map[string]*fileOp{},
		ended: map[string]time.Time{},
	}
	var saved []persistedExec
	if err := st.LoadExecs(&saved); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	now := d.now()
	for _, p := range saved {
		if now.Sub(p.IDTime) < persistedExecRetention {
			// Requests are required to spell exec_id in lowercase, but an
			// earlier agent version accepted any case and may have persisted
			// a mixed-case record. Reserve its lowercase spelling, so that
			// the lowercase request cannot start the command a second time.
			// The persisted record keeps the spelling it was written with.
			key := strings.ToLower(p.ExecID)
			if _, taken := d.lost[key]; taken && p.ExecID != key {
				continue
			}
			d.lost[key] = p
		}
	}
	if len(saved) > 0 {
		d.logger.Info("loaded exec records of an earlier agent process", "count", len(d.lost))
	}
	return d, d.persistLocked()
}

// persistLocked writes the exec records. The caller holds d.mu or has
// exclusive access.
func (d *Dispatcher) persistLocked() error {
	records := make([]persistedExec, 0, len(d.execs)+len(d.lost))
	for id, e := range d.execs {
		records = append(records, persistedExec{ExecID: id, SandboxID: e.sandboxID, Digest: e.digest, IDTime: e.idTime})
	}
	for _, p := range d.lost {
		records = append(records, p)
	}
	return d.st.SaveExecs(records)
}

// Close stops the guest forwarding of every exec.
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
}

// HasRunningExec reports whether an exec of the sandbox has not ended; a
// running exec keeps the sandbox active.
func (d *Dispatcher) HasRunningExec(sandboxID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.execs {
		if e.sandboxID == sandboxID && e.log.Final() == nil {
			return true
		}
	}
	return false
}

// ExecEnded reports whether the exec with execID was accepted and has
// ended, so the data plane can release a relay grant whose stream closed
// before the exec ended. An exec of an earlier agent process counts as
// ended; an unknown exec_id does not.
func (d *Dispatcher) ExecEnded(execID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.execs[execID]; ok {
		return e.log.Final() != nil
	}
	if _, ok := d.lost[execID]; ok {
		return true
	}
	_, ok := d.ended[execID]
	return ok
}

// SandboxEnded ends every exec of the sandbox that has not ended with a
// sandbox_ended event and stops its guest forwarding.
func (d *Dispatcher) SandboxEnded(sandboxID string, reason ownsandv1.SandboxEndReason) {
	d.mu.Lock()
	var ops []*execOp
	for _, e := range d.execs {
		if e.sandboxID == sandboxID {
			ops = append(ops, e)
		}
	}
	d.mu.Unlock()
	for _, e := range ops {
		e.log.Finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded{SandboxEnded: &ownsandv1.ExecSandboxEnded{Reason: reason}}}, d.now())
		e.cancel()
	}
}

// Sweep forgets execs 10 minutes after they ended, file operations 15
// minutes after their last use and ended IDs after 30 minutes.
func (d *Dispatcher) Sweep() {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for id, e := range d.execs {
		if end := e.log.EndTime(); !end.IsZero() && now.Sub(end) >= execlog.Retention {
			delete(d.execs, id)
			d.ended[id] = now
			// Keep the at-most-once record while the exec_id can still pass
			// the admission window.
			if now.Sub(e.idTime) < persistedExecRetention {
				d.lost[id] = persistedExec{ExecID: id, SandboxID: e.sandboxID, Digest: e.digest, IDTime: e.idTime}
			}
			changed = true
		}
	}
	for id, p := range d.lost {
		if now.Sub(p.IDTime) >= persistedExecRetention {
			delete(d.lost, id)
			changed = true
		}
	}
	for _, m := range []map[string]*fileOp{d.reads, d.writes} {
		for id, op := range m {
			if op.open == 0 && now.Sub(op.lastUse) >= fileOpRetention {
				delete(m, id)
				d.ended[id] = now
			}
		}
	}
	for id, at := range d.ended {
		if now.Sub(at) >= datapath.EndedOperationMemory {
			delete(d.ended, id)
		}
	}
	if changed {
		if err := d.persistLocked(); err != nil {
			d.logger.Warn("persist exec records failed", "error", err)
		}
	}
}

// Run sweeps every 30 seconds until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Sweep()
		}
	}
}

func idTime(id string) time.Time {
	t, _ := uuidv7.TimeOf(id)
	return t
}
