// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
)

func TestInstallationWebhookIgnoresStaleSuspension(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	eventually(t, "a waiting runner", func() bool { return len(w.runnersIn(t, waiting)) == 1 })
	runnerID := w.runnersIn(t, waiting)[0].GetRunnerId()

	old := w.gh.SendLost("installation", installationEvent("suspend", 501, "renamed"))
	w.send(t, "installation", installationEvent("unsuspend", 501, "renamed"))
	if n := w.l.GitHub.RedeliverOnce(t.Context()); n != 1 {
		t.Fatalf("redelivery requests = %d, want 1", n)
	}
	eventually(t, "successful stale suspension redelivery", func() bool {
		for _, d := range w.gh.Deliveries() {
			if d.GUID == old && d.Redelivery && d.StatusCode == http.StatusNoContent {
				return true
			}
		}
		return false
	})
	if req := w.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("stale suspension changed queued work: %v", req)
	}
	if got := w.runner(t, runnerID).GetState(); got != waiting {
		t.Fatalf("runner state = %v, want %v", got, waiting)
	}
	w.send(t, "workflow_job", job("queued", 501, 2, sandRunLinux, ""))
	if req := w.requestOfJob(t, 2); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("installation did not admit a matching job: %v", req)
	}
}

func TestInstallationWebhookLookupFailureCanRetry(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))

	w.gh.SetSuspended(501, true)
	w.gh.Update(func() { w.gh.FailInstallationLookup = http.StatusServiceUnavailable })
	body, err := json.Marshal(installationEvent("suspend", 501, "renamed"))
	if err != nil {
		t.Fatal(err)
	}
	const deliveryID = "suspension-retry"
	headers := w.gh.Headers("installation", deliveryID, body)
	if status := w.gh.SendRaw(headers, body); status != http.StatusInternalServerError {
		t.Fatalf("failed lookup webhook status = %d, want 500", status)
	}
	var recorded bool
	err = w.env.DB.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM webhook_deliveries WHERE delivery_id = $1)", deliveryID).Scan(&recorded)
	if err != nil || recorded {
		t.Fatalf("failed delivery recorded = %v, query error = %v", recorded, err)
	}
	const active = sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE
	const suspended = sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED
	checkLink := func(wantState sandrunv1.GitHubInstallationState, wantLogin string) {
		t.Helper()
		var state int16
		var login string
		err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT state, github_account_login FROM github_installations WHERE github_installation_id = 501").Scan(&state, &login)
		if err != nil || state != int16(wantState) || login != wantLogin {
			t.Fatalf("installation state = %d, login = %q, error = %v, want %v and %q", state, login, err, wantState, wantLogin)
		}
	}
	checkLink(active, "acme")
	if req := w.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("failed lookup changed queued work: %v", req)
	}

	for range 2 {
		if status := w.gh.SendRaw(headers, body); status != http.StatusNoContent {
			t.Fatalf("retry or duplicate status = %d, want 204", status)
		}
	}
	if got := w.webhookCounts(t)[string(webhook.ResultDuplicate)]; got != 1 {
		t.Errorf("duplicates = %d, want 1", got)
	}
	if got := w.gh.Calls("get_installation"); got != 2 {
		t.Errorf("installation lookups = %d, want 2", got)
	}
	checkLink(suspended, "renamed")
	if req := w.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
		req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
		t.Fatalf("successful suspension did not cancel queued work: %v", req)
	}
	w.send(t, "installation", installationEvent("unsuspend", 501, "renamed"))
	checkLink(suspended, "renamed")
	w.send(t, "workflow_job", job("queued", 501, 2, sandRunLinux, ""))
	if req := w.requestOfJob(t, 2); req != nil {
		t.Fatalf("suspended installation admitted work: %v", req)
	}
	w.gh.SetSuspended(501, false)
	w.send(t, "installation", installationEvent("unsuspend", 501, "renamed"))
	checkLink(active, "renamed")
	w.send(t, "workflow_job", job("queued", 501, 3, sandRunLinux, ""))
	if req := w.requestOfJob(t, 3); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("restored installation did not admit work: %v", req)
	}
}

func TestInstallationWebhookConcurrentOrganizationDeletion(t *testing.T) {
	t.Parallel()
	for _, deletionFirst := range []bool{true, false} {
		name := "webhook first"
		if deletionFirst {
			name = "deletion first"
		}
		t.Run(name, func(t *testing.T) {
			w := newGHWorld(t, worldOpts{manualDispatch: true})
			w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", 0))
			w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
			w.gh.SetSuspended(501, true)
			requestID := w.requestOfJob(t, 1).GetRequestId()

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			pool := w.env.DB.Pool
			barrier, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Rollback(context.Background()) }()
			barrierPID := barrier.Conn().PgConn().PID()
			deleted := make(chan error, 1)
			deleteOrg := func() {
				_, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: w.org.ID}))
				deleted <- err
			}
			body, err := json.Marshal(installationEvent("suspend", 501, "renamed"))
			if err != nil {
				t.Fatal(err)
			}
			headers := w.gh.Headers("installation", "concurrent-installation", body)
			delivered := make(chan int, 1)
			deliver := func() { delivered <- w.gh.SendRaw(headers, body) }

			wantResult := webhook.ResultProcessed
			if deletionFirst {
				// Pause the final DELETE after it has locked the organization
				// and updated the queued request, before it cascades to the link.
				if _, err := pool.Exec(ctx, `
					CREATE FUNCTION pause_organization_delete() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						PERFORM pg_advisory_xact_lock(501);
						RETURN OLD;
					END;
					$$;
					CREATE TRIGGER pause_organization_delete BEFORE DELETE ON organizations
						FOR EACH ROW EXECUTE FUNCTION pause_organization_delete()
				`); err != nil {
					t.Fatal(err)
				}
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_xact_lock(501)"); err != nil {
					t.Fatal(err)
				}
				go deleteOrg()
				deletionPID, _ := waitForInstallationTestBlock(t, w, ctx, barrierPID, "deletion at the cascade barrier")
				go deliver()
				_, query := waitForInstallationTestBlock(t, w, ctx, deletionPID, "webhook behind deletion")
				if !strings.Contains(query, "FROM organizations") || !strings.Contains(query, "FOR KEY SHARE") {
					t.Errorf("webhook waited on %q, want the organization key-share lock", query)
				}
				wantResult = webhook.ResultIgnored
			} else {
				if _, err := barrier.Exec(ctx, "SELECT 1 FROM requests WHERE request_id = $1 FOR UPDATE", requestID); err != nil {
					t.Fatal(err)
				}
				go deliver()
				webhookPID, _ := waitForInstallationTestBlock(t, w, ctx, barrierPID, "webhook at the request lock")
				go deleteOrg()
				_, query := waitForInstallationTestBlock(t, w, ctx, webhookPID, "deletion behind the webhook")
				if !strings.Contains(query, "FROM organizations") || !strings.Contains(query, "FOR UPDATE") {
					t.Errorf("deletion waited on %q, want the organization update lock", query)
				}
			}
			if err := barrier.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case status := <-delivered:
				if status != http.StatusNoContent {
					t.Errorf("webhook status %d, want 204", status)
				}
			case <-ctx.Done():
				t.Fatal("webhook did not finish after releasing the barrier")
			}
			if err := <-deleted; err != nil {
				t.Fatalf("organization deletion: %v", err)
			}
			if status := w.gh.SendRaw(headers, body); status != http.StatusNoContent {
				t.Fatalf("duplicate status %d, want 204", status)
			}
			counts := w.webhookCounts(t)
			want := int64(1)
			if wantResult == webhook.ResultProcessed {
				want++ // The setup workflow_job was also processed.
			}
			if counts[string(wantResult)] != want || counts[string(webhook.ResultDuplicate)] != 1 {
				t.Errorf("delivery counts %v, want %d %s and one duplicate", counts, want, wantResult)
			}
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM organizations WHERE organization_id = $1)
				OR EXISTS (SELECT 1 FROM github_installations WHERE organization_id = $1)
				OR EXISTS (SELECT 1 FROM requests WHERE organization_id = $1)`, w.org.ID).Scan(&exists); err != nil || exists {
				t.Errorf("organization or dependent rows remain: %v (query error %v)", exists, err)
			}
		})
	}
}

func TestInstallationWebhookRechecksLockedLink(t *testing.T) {
	t.Parallel()
	const (
		active  = sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE
		deleted = sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED
	)
	for _, name := range []string{"removed", "reassigned", "deleted state"} {
		t.Run(name, func(t *testing.T) {
			w := newGHWorld(t, worldOpts{manualDispatch: true})
			w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", 0))
			w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
			owner := w.org.ID
			if name == "reassigned" {
				owner = w.env.NewOrg("bob").ID
			}
			ctx := t.Context()
			pool := w.env.DB.Pool
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := api.LockOrganization(ctx, tx, w.org.ID); err != nil {
				t.Fatal(err)
			}
			login, wantLogin := "renamed", "acme"
			if name == "deleted state" {
				// Skip the independent account-login refresh for this check.
				login = wantLogin
			}
			delivered := make(chan int, 1)
			go func() {
				_, status := w.gh.Send("installation", installationEvent("suspend", 501, login))
				delivered <- status
			}()
			waitForInstallationTestBlock(t, w, ctx, tx.Conn().PgConn().PID(), "webhook at the organization lock")

			wantState := active
			switch name {
			case "removed":
				_, err = tx.Exec(ctx, "DELETE FROM github_installations WHERE github_installation_id = 501")
			case "reassigned":
				_, err = tx.Exec(ctx, "UPDATE github_installations SET organization_id = $1 WHERE github_installation_id = 501", owner)
			case "deleted state":
				wantState = deleted
				_, err = tx.Exec(ctx, "UPDATE github_installations SET state = $1 WHERE github_installation_id = 501", int16(deleted))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if status := <-delivered; status != http.StatusNoContent {
				t.Fatalf("webhook status %d, want 204", status)
			}
			if got := w.webhookCounts(t)[string(webhook.ResultIgnored)]; got != 1 {
				t.Errorf("ignored deliveries %d, want 1", got)
			}
			var gotOwner, gotLogin string
			var gotState int16
			err = pool.QueryRow(ctx, `SELECT organization_id::text, github_account_login, state
				FROM github_installations WHERE github_installation_id = 501`).Scan(&gotOwner, &gotLogin, &gotState)
			if name == "removed" {
				if err != pgx.ErrNoRows {
					t.Errorf("removed link lookup returned %v, want no rows", err)
				}
			} else if err != nil || gotOwner != owner || gotLogin != wantLogin || gotState != int16(wantState) {
				t.Errorf("link = (%s, %s, %d), want (%s, %s, %d), query error %v", gotOwner, gotLogin, gotState, owner, wantLogin, wantState, err)
			}
			if req := w.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
				t.Errorf("webhook changed queued work: %v", req)
			}
		})
	}
}

func TestInstallationWebhookRedeliversAfterLockFailure(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	w.gh.SetSuspended(501, true)
	ctx := t.Context()
	pool := w.env.DB.Pool
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := api.LockOrganization(ctx, tx, w.org.ID); err != nil {
		t.Fatal(err)
	}

	delivered := make(chan int, 1)
	go func() {
		_, status := w.gh.Send("installation", installationEvent("suspend", 501, "acme"))
		delivered <- status
	}()
	pid, _ := waitForInstallationTestBlock(t, w, ctx, tx.Conn().PgConn().PID(), "webhook at the organization lock")
	var canceled bool
	if err := pool.QueryRow(ctx, "SELECT pg_cancel_backend($1)", pid).Scan(&canceled); err != nil || !canceled {
		t.Fatalf("cancel database wait: canceled %v, error %v", canceled, err)
	}
	if status := <-delivered; status != http.StatusInternalServerError {
		t.Fatalf("webhook status %d, want 500", status)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if req := w.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("failed delivery changed queued work: %v", req)
	}
	if n := w.l.GitHub.RedeliverOnce(ctx); n != 1 {
		t.Fatalf("redelivery requests %d, want 1", n)
	}
	eventually(t, "redelivery to cancel queued work", func() bool {
		return w.requestOfJob(t, 1).GetEndReason() == sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED
	})
}

func waitForInstallationTestBlock(t *testing.T, w *ghWorld, ctx context.Context, blocker uint32, what string) (uint32, string) {
	t.Helper()
	var pid uint32
	var query string
	eventually(t, what, func() bool {
		// The query text comes from the pg_stat_activity snapshot of this
		// statement, but pg_blocking_pids reads the lock manager later. A
		// backend can run a statement when the snapshot is taken, move on and
		// block on the next one before pg_blocking_pids runs, so the row would
		// show the earlier statement. The wait event comes from the same
		// snapshot as the query, so requiring a lock wait keeps only rows whose
		// query is the statement that waits.
		err := w.env.DB.Pool.QueryRow(ctx, `
			SELECT pid, query FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND $1 = ANY(pg_blocking_pids(pid))
		`, blocker).Scan(&pid, &query)
		if err == pgx.ErrNoRows {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	})
	return pid, query
}
