// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func seedSweepDevice(t *testing.T, env *testenv.Env, org *testenv.Org, state ownsandv1.DeviceState, seen *time.Time) string {
	t.Helper()
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	setSweepDevice(t, env, dev.ID, state, seen)
	return dev.ID
}

func setSweepDevice(t *testing.T, env *testenv.Env, id string, state ownsandv1.DeviceState, seen *time.Time) {
	t.Helper()
	if _, err := env.DB.Pool.Exec(context.Background(), "UPDATE devices SET state = $2, last_seen_time = $3 WHERE device_id = $1",
		id, int16(state), seen); err != nil {
		t.Fatal(err)
	}
}

func seedSweepSandbox(t *testing.T, env *testenv.Env, org *testenv.Org, deviceID string,
	state ownsandv1.SandboxState, reason *int16,
) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Pool.Exec(context.Background(), `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, end_reason, vcpu_count, memory_mib, disk_gib,
		guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, max_deadline, created_by_kind, created_by_id, create_time)
		VALUES ($1, $2, $3, $4, $5, 1, 512, 1, 1, 1, 1, 1, 60000, 60000, now() + interval '1 hour', 1, 'alice', now())`,
		id, org.ID, deviceID, int16(state), reason); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOfflineSweepEligibility(t *testing.T) {
	t.Parallel()
	for _, registry := range []string{"empty", "populated"} {
		t.Run(registry, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			now := env.Clock.Now()
			old := now.Add(-time.Hour)
			online := ownsandv1.DeviceState_DEVICE_STATE_ONLINE
			offline := ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
			removed := ownsandv1.DeviceState_DEVICE_STATE_REMOVED
			running := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
			deleted := ownsandv1.SandboxState_SANDBOX_STATE_DELETED

			wantDevices := make(map[string]ownsandv1.DeviceState)
			if registry == "populated" {
				live, _ := setupAgent(t, env, org, nil)
				setSweepDevice(t, env, live.ID, online, &old)
				wantDevices[live.ID] = online
			}
			recent := seedSweepDevice(t, env, org, online, &now)
			removedID := seedSweepDevice(t, env, org, removed, &old)
			stale := seedSweepDevice(t, env, org, online, &old)
			unheard := seedSweepDevice(t, env, org, online, nil)
			wantDevices[recent], wantDevices[removedID] = online, removed
			wantDevices[stale], wantDevices[unheard] = offline, offline

			staleRunning := seedSweepSandbox(t, env, org, stale, running, nil)
			unheardRunning := seedSweepSandbox(t, env, org, unheard, running, nil)
			recentRunning := seedSweepSandbox(t, env, org, recent, running, nil)
			endReason := int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
			terminal := seedSweepSandbox(t, env, org, stale, deleted, &endReason)

			for _, id := range []string{stale, unheard} {
				testenv.Eventually(t, "committed offline transition for "+id, func() bool {
					for _, rec := range deviceOfflineLogs(t, env, id) {
						if rec.Msg == "device_offline" {
							return true
						}
					}
					return false
				})
			}
			for id, want := range wantDevices {
				if got := deviceState(t, org, id); got != want {
					t.Errorf("device %s = %s, want %s", id, got, want)
				}
			}
			for _, id := range []string{staleRunning, unheardRunning} {
				sb := getSandbox(t, org.OwnerClient, org.ID, id)
				if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE || sb.GetLastKnownState() != running {
					t.Errorf("sandbox %s = %s, last known %s", id, sb.GetState(), sb.GetLastKnownState())
				}
			}
			if sb := getSandbox(t, org.OwnerClient, org.ID, recentRunning); sb.GetState() != running {
				t.Errorf("sandbox on recent device = %s, want RUNNING", sb.GetState())
			}
			if sb := getSandbox(t, org.OwnerClient, org.ID, terminal); sb.GetState() != deleted || int16(sb.GetEndReason()) != endReason {
				t.Errorf("terminal sandbox = %s, end reason %s", sb.GetState(), sb.GetEndReason())
			}
			t.Log("GetDevice and GetSandbox confirmed the sweep changed only eligible devices and their nonterminal sandboxes")
		})
	}
}

func waitingSweepPID(t *testing.T, env *testenv.Env, pattern string) int {
	t.Helper()
	var pid int
	testenv.Eventually(t, "the sweep waits for "+pattern, func() bool {
		err := env.DB.Pool.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND state = 'active' AND wait_event_type = 'Lock' AND query LIKE $1`, pattern).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	})
	return pid
}

func waitForSweepTransactionEnd(t *testing.T, env *testenv.Env, pid int) {
	t.Helper()
	testenv.Eventually(t, "the old sweep transaction ends", func() bool {
		var active bool
		if err := env.DB.Pool.QueryRow(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND xact_start IS NOT NULL)", pid).Scan(&active); err != nil {
			t.Fatal(err)
		}
		return !active
	})
}

func TestOfflineSweepDuringLeaseLossWhileDeviceRowHeld(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"takeover", "reacquisition"} {
		t.Run(scenario, func(t *testing.T) {
			env := testenv.New(t)
			env.WaitForLease()
			peer := env.StartPeer()
			org := env.NewOrg("alice")
			now := env.Clock.Now()
			online := ownsandv1.DeviceState_DEVICE_STATE_ONLINE
			running := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING

			var oldPeriod *lease.Period
			if !env.App.Lease.WhileHeld(func(p *lease.Period) { oldPeriod = p }) {
				t.Fatal("the original holder lost its lease before the sweep")
			}
			id := seedSweepDevice(t, env, org, online, &now)
			sb := seedSweepSandbox(t, env, org, id, running, nil)
			releaseDevice := holdDeviceLock(t, env, id)
			defer releaseDevice()
			env.Clock.StepWall(time.Hour)
			// Matches both the unfenced UPDATE and the fenced row selection.
			pid := waitingSweepPID(t, env, "%devices%last_seen_time%")
			// Only the pass already waiting on the row can see this device
			// as stale. A pass from a later period cannot mask its result.
			env.Clock.StepWall(-time.Hour)

			released := make(chan struct{})
			go func() {
				env.App.Lease.Release()
				close(released)
			}()
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("lease release waited for the device row")
			}

			holder := peer
			if scenario == "takeover" {
				runHandoverLease(t, peer)
			} else {
				holder = env
				// The original instance keeps running. Its next normal
				// renewal attempt acquires a distinct period.
				ctx, cancel := context.WithTimeout(context.Background(), lease.RenewInterval+5*time.Second)
				defer cancel()
				if err := holder.App.Lease.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var current *lease.Period
			if !holder.App.Lease.WhileHeld(func(p *lease.Period) { current = p }) || current == nil || current == oldPeriod {
				t.Fatal("no new lease period while the device row remained locked")
			}
			t.Log("lease released and a new period acquired while the original device row remained locked")

			releaseDevice()
			waitForSweepTransactionEnd(t, env, pid)
			org.OwnerClient = holder.User(org.Owner)
			if got := deviceState(t, org, id); got != online {
				t.Errorf("device state after the old sweep = %s, want ONLINE", got)
			}
			if got := getSandbox(t, org.OwnerClient, org.ID, sb).GetState(); got != running {
				t.Errorf("sandbox state after the old sweep = %s, want RUNNING", got)
			}
			if strings.Contains(env.Logs.String(), `"msg":"offline_sweep_failed"`) || len(deviceOfflineLogs(t, env, id)) != 0 {
				t.Error("the superseded sweep logged a failure or a device transition")
			}
		})
	}
}

func TestOfflineSweepChecksDatabaseExpiryAtWrite(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	env.WaitForLease()
	org := env.NewOrg("alice")
	now := env.Clock.Now()
	online := ownsandv1.DeviceState_DEVICE_STATE_ONLINE
	id := seedSweepDevice(t, env, org, online, &now)

	ctx := context.Background()
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM instance_leases WHERE lease_name = $1 FOR UPDATE", lease.Name); err != nil {
		t.Fatal(err)
	}
	env.Clock.StepWall(time.Hour)
	pid := waitingSweepPID(t, env, "%FROM instance_leases%FOR SHARE%")
	env.Clock.StepWall(-time.Hour)
	// Expire only the shared row, while the local period is still current.
	if _, err := tx.Exec(ctx, "UPDATE instance_leases SET expire_time = clock_timestamp() WHERE lease_name = $1", lease.Name); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitForSweepTransactionEnd(t, env, pid)
	if !env.App.Lease.Held() {
		t.Fatal("local lease ended before the expiry check could be observed")
	}
	if got := deviceState(t, org, id); got != online {
		t.Errorf("device state after database lease expiry = %s, want ONLINE", got)
	}
	if strings.Contains(env.Logs.String(), `"msg":"offline_sweep_failed"`) || len(deviceOfflineLogs(t, env, id)) != 0 {
		t.Error("the expired sweep logged a failure or a device transition")
	}
	t.Log("GetDevice stayed ONLINE after the shared lease row expired and the local period remained current")
}

func TestOfflineSweepLogsCommitFailure(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	org := env.NewOrg("alice")
	now := env.Clock.Now()
	online := ownsandv1.DeviceState_DEVICE_STATE_ONLINE
	id := seedSweepDevice(t, env, org, online, &now)
	// Reject the transaction at commit, after the UPDATE returns the ID.
	if _, err := env.DB.Pool.Exec(context.Background(), `
		CREATE FUNCTION reject_sweep_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'fixture rejected sweep commit';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER reject_sweep_commit AFTER UPDATE ON devices
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_sweep_commit()
	`); err != nil {
		t.Fatal(err)
	}
	env.Clock.StepWall(time.Hour)
	testenv.Eventually(t, "the sweep reports the commit failure", func() bool {
		logs := env.Logs.String()
		return strings.Contains(logs, `"msg":"offline_sweep_failed"`) && strings.Contains(logs, "fixture rejected sweep commit")
	})
	if got := deviceState(t, org, id); got != online {
		t.Errorf("device state after rollback = %s, want ONLINE", got)
	}
	if records := deviceOfflineLogs(t, env, id); len(records) != 0 {
		t.Errorf("rolled-back sweep logged a device transition: %v", records)
	}
}
