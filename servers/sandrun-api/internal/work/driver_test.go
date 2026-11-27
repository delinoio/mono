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

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/link"
	"github.com/delinoio/mono/servers/sandrun-api/internal/secrets"
)

func TestSaveStartResultCancelsRetry(t *testing.T) {
	// A closed pool fails the write without connecting to PostgreSQL.
	pool, err := pgxpool.New(t.Context(), "postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		e := New(&api.Service{DB: &db.DB{Pool: pool}, Logger: slog.New(slog.NewTextHandler(&logs, nil))},
			nil, nil, nil, Timing{SandboxPoll: time.Hour})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.saveStartResult(ctx, &Runner{ID: "runner"}, StartResult{ProgramExecID: "exec"})
		}()

		synctest.Wait()
		if !strings.Contains(logs.String(), "runner_update_failed") {
			t.Fatal("the start outcome transaction did not fail")
		}
		select {
		case <-done:
			t.Fatal("the driver exited before retrying")
		default:
		}
		start := time.Now()
		cancel()
		<-done
		if spent := time.Since(start); spent != 0 || strings.Count(logs.String(), "runner_update_failed") != 1 {
			t.Fatalf("cancellation took %v and logged %d failures, want immediate exit after one write attempt",
				spent, strings.Count(logs.String(), "runner_update_failed"))
		}
	})
}

func TestProgramFollowerCancelsLinkRetry(t *testing.T) {
	// A closed pool fails the active-link lookup without connecting to PostgreSQL.
	pool, err := pgxpool.New(t.Context(), "postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	var e *Engine
	_, err = app.New(app.Options{
		DB: &db.DB{Pool: pool}, Logger: slog.New(slog.DiscardHandler), Secrets: &secrets.Keyring{},
		Extensions: []app.Extension{func(r *app.Registry) error {
			links := link.Register(r, nil, nil, link.RemovalOptions{})
			e = New(r.Service, links, nil, nil, Timing{SandboxPoll: time.Hour})
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.followProgram(ctx, &Runner{OrganizationID: "org", ID: "runner", SandboxID: "sandbox", ProgramExecID: "exec"})
		}()

		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the follower exited after the lookup failure")
		default:
		}
		start := time.Now()
		cancel()
		<-done
		if spent := time.Since(start); spent != 0 {
			t.Fatalf("cancellation took %v, want immediate exit", spent)
		}
	})
}
