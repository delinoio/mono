// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// Control runs the lease holder's background work: the control event
// processor, the offline sweep and the R2 clean-up worker.
type Control struct {
	S *Service

	kick chan struct{}

	mu sync.Mutex
	// cursor is the last processed control event; -1 means unset.
	cursor int64
	// cursorGen counts the lease transitions that stored the cursor. Every
	// LeaseAcquired and LeaseLost increments it in the same critical section
	// as its cursor store, even when the stored value is unchanged. A batch
	// in processEvents copies it together with the cursor and stops as soon
	// as it differs, so a batch that started under an earlier lease period
	// never delivers an event or stores a cursor in a later one.
	cursorGen uint64

	// Seams of the event processor. NewControl sets them to the database
	// functions; tests replace them to interleave lease transitions with a
	// batch deterministically.
	latestID    func(ctx context.Context) (int64, error)
	readEvents  func(ctx context.Context, after int64, limit int) ([]events.Event, error)
	handleEvent func(ctx context.Context, e events.Event)
}

// NewControl returns the background worker of s and installs s.Notify.
func NewControl(s *Service) *Control {
	c := &Control{S: s, kick: make(chan struct{}, 1), cursor: -1}
	c.latestID = func(ctx context.Context) (int64, error) { return events.LatestID(ctx, s.DB.Pool) }
	c.readEvents = func(ctx context.Context, after int64, limit int) ([]events.Event, error) {
		return events.After(ctx, s.DB.Pool, after, limit)
	}
	c.handleEvent = c.handle
	s.Notify = c.Kick
	return c
}

// Kick asks the event processor to poll now.
func (c *Control) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// LeaseAcquired starts the event processor at the newest event: events
// older than the lease belong to channels of the previous holder, and the
// state reports of reconnecting agents trigger pending deletes again. The
// lease cancels ctx at the confirmed expiry of the ownership period, so the
// database read cannot delay the loss transition; a failed read leaves the
// cursor unset and the event processor repeats it while the lease is held.
func (c *Control) LeaseAcquired(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, err := c.latestID(ctx)
	if err != nil {
		c.S.Logger.Warn("control_event_cursor_failed", slog.Any("error", err))
		id = -1
	}
	c.mu.Lock()
	c.cursor = id
	c.cursorGen++
	c.mu.Unlock()
}

// initCursor sets an unset cursor to the newest event for the event processor
// batch that copied generation gen. Unlike LeaseAcquired it is not a lease
// transition: it reads the newest event ID before it takes the lock and
// stores it only when no transition ran since the batch copied gen and the
// cursor is still unset, and it never increments cursorGen. A read that
// started in an earlier lease period therefore cannot replace the cursor that
// a later LeaseAcquired stored, or set a cursor that a later LeaseLost
// cleared. A failed read leaves the cursor and the generation unchanged and
// the event processor repeats it while the lease is held.
func (c *Control) initCursor(ctx context.Context, gen uint64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, err := c.latestID(ctx)
	if err != nil {
		c.S.Logger.Warn("control_event_cursor_failed", slog.Any("error", err))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cursorGen != gen || c.cursor >= 0 {
		c.S.Logger.Info("control_event_batch_superseded", slog.Int64("cursor", c.cursor))
		return
	}
	c.cursor = id
}

// ProcessedCursor returns the delivery ID of the last control event that this
// instance processed as the lease holder. ok is false when the cursor is
// unset: LeaseLost clears it, so an instance that is not the lease holder
// reports false, and so does a holder whose cursor read failed. The janitor
// deletes control events only up to this cursor, so an event that no lease
// holder has processed is never deleted by its age. A cursor that outlives
// the lease by the moment before LeaseLost runs only covers events that this
// instance processed, so the janitor cannot delete an unprocessed event then
// either.
func (c *Control) ProcessedCursor() (id int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor, c.cursor >= 0
}

// LeaseLost closes every agent channel; agents reconnect to the new holder.
func (c *Control) LeaseLost() {
	c.S.Hub.CloseAll(errLeaseLost)
	c.mu.Lock()
	c.cursor = -1
	c.cursorGen++
	c.mu.Unlock()
}

var errLeaseLost = leaseLostError{}

type leaseLostError struct{}

func (leaseLostError) Error() string { return "the instance released the lease" }

// Run runs the workers until ctx ends.
func (c *Control) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.runEvents(ctx) }()
	go func() { defer wg.Done(); c.runOfflineSweep(ctx) }()
	go func() { defer wg.Done(); c.runCleanup(ctx) }()
	wg.Wait()
}

func (c *Control) held() bool { return c.S.Lease != nil && c.S.Lease.Held() }

func (c *Control) runEvents(ctx context.Context) {
	t := time.NewTicker(c.S.Timing.EventPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.kick:
		}
		if c.held() {
			c.processEvents(ctx)
		}
	}
}

// processEvents delivers the events after the cursor; an unset cursor is
// first initialized by initCursor. The cursor belongs to
// the lease period in which the batch started: when LeaseLost or
// LeaseAcquired runs while the batch is in flight, the batch stops before
// its next read, its next delivery and its cursor store, so it neither
// delivers an event of a newer period nor replaces the cursor that
// LeaseAcquired stored. Deliveries that already returned are not undone.
func (c *Control) processEvents(ctx context.Context) {
	c.mu.Lock()
	cursor, gen := c.cursor, c.cursorGen
	c.mu.Unlock()
	if cursor < 0 {
		c.initCursor(ctx, gen)
		return
	}
	for {
		if !c.cursorCurrent(gen) {
			return
		}
		batch, err := c.readEvents(ctx, cursor, 500)
		if err != nil {
			if ctx.Err() == nil {
				c.S.Logger.Warn("control_events_read_failed", slog.Any("error", err))
			}
			return
		}
		for _, e := range batch {
			if !c.cursorCurrent(gen) {
				c.S.Logger.Info("control_event_batch_superseded", slog.Int64("cursor", cursor))
				return
			}
			c.handleEvent(ctx, e)
			cursor = e.ID
		}
		c.mu.Lock()
		if c.cursorGen != gen {
			c.mu.Unlock()
			c.S.Logger.Info("control_event_batch_superseded", slog.Int64("cursor", cursor))
			return
		}
		c.cursor = cursor
		c.mu.Unlock()
		if len(batch) < 500 {
			return
		}
	}
}

// cursorCurrent reports whether no lease transition stored the cursor since
// the batch copied generation gen.
func (c *Control) cursorCurrent(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursorGen == gen
}

func (c *Control) handle(ctx context.Context, e events.Event) {
	s := c.S
	switch e.Kind {
	case events.KindDeviceConfigChanged:
		if conn := s.Hub.Get(e.DeviceID); conn != nil {
			_ = s.sendConfiguration(ctx, conn)
		}
	case events.KindAgentPolicyChanged:
		for _, conn := range s.Hub.All() {
			if conn.OrganizationID == e.OrganizationID && conn.Reconciled() {
				if err := s.evaluateUpdate(ctx, conn); err != nil {
					s.Logger.Warn("agent_update_evaluation_failed", slog.String("device_id", conn.DeviceID), slog.Any("error", err))
				}
			}
		}
	case events.KindSandboxDeleteRequested:
		conn := s.Hub.Live(e.DeviceID)
		if conn == nil {
			return
		}
		sb, err := loadSandbox(ctx, s.DB.Pool, e.OrganizationID, e.SandboxID, false)
		if err != nil || isTerminal(sb.AgentState) || !sb.DeletePending {
			return
		}
		s.sendDelete(ctx, conn, e.OrganizationID, pendingDelete{sandboxID: sb.ID, reason: sb.DeleteEndReason})
	case events.KindSnapshotDeleted:
		if conn := s.Hub.Live(e.DeviceID); conn != nil {
			s.sendDeleteSnapshot(ctx, conn, e.OrganizationID, e.SnapshotID)
		}
	case events.KindDeviceRemoved:
		conn := s.Hub.Get(e.DeviceID)
		if conn == nil {
			return
		}
		// The clean-up command goes out first; the nil envelope makes the
		// writer close the channel after it.
		if _, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdCleanUp, orgID: e.OrganizationID},
			&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}}}); err == nil {
			_ = conn.Send(ctx, nil)
		}
	}
	if s.DataPath != nil {
		switch e.Kind {
		case events.KindAPIKeyRevoked, events.KindMembershipEnded, events.KindSandboxDeleted, events.KindOrganizationDeleted, events.KindDeviceRemoved:
			s.DataPath.HandleEvent(ctx, e)
		}
	}
}

// expireCommands removes the command records whose channel closed longer
// ago than the command grace period. It changes no durable state and
// synthesizes no command failure: the result of an expired command was lost,
// and the agent's next state report reconciles the sandbox as usual.
func (s *Service) expireCommands(ctx context.Context) {
	if n := s.commands.expire(s.now(), s.commandGrace()); n > 0 {
		s.Logger.InfoContext(ctx, "agent_commands_expired", slog.Int("commands", n), slog.Duration("grace", s.commandGrace()))
	}
}

// runOfflineSweep marks devices offline that are ONLINE in the database
// but have no channel on the lease holder and were not heard from for the
// heartbeat timeout (for example after a lease handover or a crash).
func (c *Control) runOfflineSweep(ctx context.Context) {
	t := time.NewTicker(c.S.Timing.OfflineSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The sweep also drops the expired command records. It runs without
		// the lease: a record outlives the lease only when its channel closed
		// at the lease release, and it must expire then too.
		c.S.expireCommands(ctx)
		// Pending revocations are bound to channels of this instance, so
		// they expire without the lease as well.
		if c.S.DataPath != nil {
			c.S.DataPath.SweepExpired(ctx)
		}
		if !c.held() {
			continue
		}
		// A non-nil slice: pgx binds a nil slice as SQL NULL, which turns
		// NOT (x = ANY(NULL)) into NULL and matches no row when the channel
		// registry is empty.
		live := []string{}
		for _, conn := range c.S.Hub.All() {
			live = append(live, conn.DeviceID)
		}
		cutoff := c.S.now().Add(-c.S.Timing.HeartbeatTimeout)
		rows, err := c.S.DB.Pool.Query(ctx, `UPDATE devices SET state = $1 WHERE state = $2 AND (last_seen_time IS NULL OR last_seen_time < $3)
			AND NOT (device_id::text = ANY($4)) RETURNING device_id::text`,
			int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE), int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE), cutoff, live)
		if err != nil {
			if ctx.Err() == nil {
				c.S.Logger.Warn("offline_sweep_failed", slog.Any("error", err))
			}
			continue
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				c.S.Logger.Info("device_offline", slog.String("device_id", id), slog.String("cause", "not heard from"))
			}
		}
		rows.Close()
	}
}

// cleanupBackoffMax bounds the retry delay of a failed R2 deletion.
const cleanupBackoffMax = time.Hour

// runCleanup runs the R2 clean-up jobs: the first sweep at once, the final
// sweep after every signed upload URL for the prefix expired, retrying
// failures with exponential backoff and logging each failure.
func (c *Control) runCleanup(ctx context.Context) {
	t := time.NewTicker(c.S.Timing.CleanupPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.S.Objects != nil {
			c.RunCleanupOnce(ctx)
		}
	}
}

// RunCleanupOnce processes the due clean-up jobs once.
func (c *Control) RunCleanupOnce(ctx context.Context) {
	s := c.S
	for {
		done := true
		err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
			var id, prefix string
			var firstDone bool
			var finalAfter time.Time
			var attempts int
			err := tx.QueryRow(ctx, `SELECT job_id::text, prefix, first_sweep_done, final_sweep_after, attempts FROM r2_cleanup_jobs
				WHERE next_attempt_time <= $1 ORDER BY next_attempt_time LIMIT 1 FOR UPDATE SKIP LOCKED`, s.now()).
				Scan(&id, &prefix, &firstDone, &finalAfter, &attempts)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if err := s.Objects.DeletePrefix(ctx, prefix); err != nil {
				attempts++
				backoff := time.Duration(1<<min(attempts, 12)) * time.Second
				if backoff > cleanupBackoffMax {
					backoff = cleanupBackoffMax
				}
				s.Logger.Warn("r2_cleanup_failed", slog.String("job_id", id), slog.Int("attempts", attempts), slog.Any("error", err))
				_, err := tx.Exec(ctx, "UPDATE r2_cleanup_jobs SET attempts = $2, next_attempt_time = $3 WHERE job_id = $1", id, attempts, s.now().Add(backoff))
				return err
			}
			if firstDone && !s.now().Before(finalAfter) {
				s.Logger.Info("r2_cleanup_completed", slog.String("job_id", id))
				_, err := tx.Exec(ctx, "DELETE FROM r2_cleanup_jobs WHERE job_id = $1", id)
				return err
			}
			next := finalAfter
			if next.Before(s.now()) {
				next = s.now()
			}
			_, err = tx.Exec(ctx, "UPDATE r2_cleanup_jobs SET first_sweep_done = true, attempts = 0, next_attempt_time = $2 WHERE job_id = $1", id, next)
			return err
		})
		if err != nil {
			if ctx.Err() == nil {
				s.Logger.Warn("r2_cleanup_job_failed", slog.Any("error", err))
			}
			return
		}
		if done {
			return
		}
	}
}
