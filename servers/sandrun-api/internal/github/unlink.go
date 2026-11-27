// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

// retireUnlinkRunners runs while lockTx holds the installation lock. The
// lock spans all retirements and the final unlink so JIT starts and webhooks
// cannot publish new registrations or bind jobs between commits.
func (l *Layer) retireUnlinkRunners(ctx context.Context, lockTx pgx.Tx, inst *Installation) ([]string, map[string]bool, error) {
	idle, err := l.idleRunners(ctx, lockTx, inst.OrganizationID, inst.PoolKey())
	if err != nil {
		return nil, nil, err
	}
	var conn *pgx.Conn
	var retired []string
	skip := map[string]bool{}
	for _, r := range idle {
		reg, err := l.registration(ctx, lockTx, r.ID)
		if err != nil {
			return retired, nil, err
		}
		if reg == nil || reg.githubRunnerID == 0 {
			continue
		}
		if conn == nil {
			// Use a separate connection so each retirement can commit without
			// releasing the installation lock. The pool can be full of
			// transactions waiting for that lock.
			conn, err = pgx.ConnectConfig(ctx, l.svc.DB.Pool.Config().ConnConfig)
			if err != nil {
				return retired, nil, err
			}
			defer conn.Close(ctx)
		}
		committed, refused, err := l.retireUnlinkRunner(ctx, conn, inst, r.ID, reg)
		if err != nil {
			return retired, nil, err
		}
		if refused {
			skip[r.ID] = true
		}
		if committed {
			retired = append(retired, r.ID)
		}
	}
	return retired, skip, nil
}

func (l *Layer) retireUnlinkRunner(ctx context.Context, conn *pgx.Conn, inst *Installation, runnerID string, expected *registration) (retired, refused bool, err error) {
	err = db.InTxWithContexts(ctx, ctx, conn.Begin, func(tx pgx.Tx) error {
		var current registration
		err := tx.QueryRow(ctx, `
			SELECT g.github_installation_id, g.github_runner_id, g.registration_removed
			FROM runners r JOIN github_runners g USING (runner_id)
			WHERE r.runner_id = $1 AND r.organization_id = $2 AND r.request_id IS NULL AND r.state = ANY($3)
			  AND g.github_runner_id IS NOT NULL
			FOR UPDATE OF r, g`, runnerID, inst.OrganizationID, idleRunnerStates).
			Scan(&current.githubInstallationID, &current.githubRunnerID, &current.removed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.githubInstallationID != inst.GitHubID || current.githubRunnerID != expected.githubRunnerID {
			return errors.New("GitHub runner registration changed during unlink")
		}
		if !current.removed {
			refused, err = l.removeKnownRegistration(ctx, tx, inst, runnerID, &current)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return api.ErrUnavailable("GitHub runner registration could not be removed; try again")
			}
			if refused {
				return nil
			}
		}
		r, err := l.engine.DeleteIdleRunnerTx(ctx, tx, inst.OrganizationID, runnerID, sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP)
		if err == nil && r == nil {
			return errors.New("idle GitHub runner changed during unlink retirement")
		}
		retired = r != nil
		return err
	})
	return err == nil && retired, refused, err
}
