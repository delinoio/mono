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
}

// NewControl returns the background worker of s and installs s.Notify.
func NewControl(s *Service) *Control {
	c := &Control{S: s, kick: make(chan struct{}, 1), cursor: -1}
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
// state reports of reconnecting agents trigger pending deletes again.
func (c *Control) LeaseAcquired() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := events.LatestID(ctx, c.S.DB.Pool)
	if err != nil {
		c.S.Logger.Warn("control_event_cursor_failed", slog.Any("error", err))
		id = -1
	}
	c.mu.Lock()
	c.cursor = id
	c.mu.Unlock()
}

// LeaseLost closes every agent channel; agents reconnect to the new holder.
func (c *Control) LeaseLost() {
	c.S.Hub.CloseAll(errLeaseLost)
	c.mu.Lock()
	c.cursor = -1
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

func (c *Control) processEvents(ctx context.Context) {
	c.mu.Lock()
	cursor := c.cursor
	c.mu.Unlock()
	if cursor < 0 {
		c.LeaseAcquired()
		return
	}
	for {
		batch, err := events.After(ctx, c.S.DB.Pool, cursor, 500)
		if err != nil {
			if ctx.Err() == nil {
				c.S.Logger.Warn("control_events_read_failed", slog.Any("error", err))
			}
			return
		}
		for _, e := range batch {
			c.handle(ctx, e)
			cursor = e.ID
		}
		c.mu.Lock()
		if c.cursor >= 0 {
			c.cursor = cursor
		}
		c.mu.Unlock()
		if len(batch) < 500 {
			return
		}
	}
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
