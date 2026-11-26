// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
)

type exitedProgramClient struct {
	ownsandclient.Client
	attachCalls int
}

func (c *exitedProgramClient) AttachExec(_ context.Context, _, execID string, _ ownsand.AttachParams) (*ownsand.ExecResult, error) {
	c.attachCalls++
	code := int64(0)
	return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
}

func TestFollowProgramCancelsExitRetry(t *testing.T) {
	// A closed pool fails the exit transaction without a database connection.
	pool, err := pgxpool.New(t.Context(), "postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		e := New(&api.Service{DB: &db.DB{Pool: pool}, Logger: slog.New(slog.NewTextHandler(&logs, nil))},
			nil, nil, nil, Timing{SandboxPoll: time.Hour})
		client := &exitedProgramClient{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.followProgram(ctx, client, &Runner{ID: "runner", SandboxID: "sandbox", ProgramExecID: "exec"})
		}()

		synctest.Wait()
		if !strings.Contains(logs.String(), "runner_program_exit_failed") {
			t.Fatal("the exit transaction did not fail")
		}
		select {
		case <-done:
			t.Fatal("the follower exited before retrying")
		default:
		}

		start := time.Now()
		cancel()
		<-done
		if spent := time.Since(start); spent != 0 || client.attachCalls != 1 {
			t.Fatalf("cancellation took %v and used %d attach calls, want immediate exit after one attach", spent, client.attachCalls)
		}
	})
}
