// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// exportClaimCommitFault drops the reply only for the first export claim.
// The direct test pool can observe the row while the API still waits for
// that reply. For an early drop, the proxy instead holds COMMIT until the
// test releases it, so the original transaction keeps its row lock after
// the API has lost its connection.
type exportClaimCommitFault struct {
	ctx       context.Context
	early     bool
	rollback  bool
	triggered atomic.Bool
	outcome   chan string
	gate      chan struct{}
	release   func()
}

func newExportClaimCommitFault(t *testing.T, early, rollback bool) *exportClaimCommitFault {
	gate := make(chan struct{})
	return &exportClaimCommitFault{
		ctx: t.Context(), early: early, rollback: rollback, outcome: make(chan string, 1),
		gate: gate, release: sync.OnceFunc(func() { close(gate) }),
	}
}

func (f *exportClaimCommitFault) install(t *testing.T, opts *app.Options) {
	t.Helper()
	cfg := opts.DB.Pool.Config()
	upstream := net.JoinHostPort(cfg.ConnConfig.Host, fmt.Sprint(cfg.ConnConfig.Port))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.WaitGroup
	connections.Go(func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Go(func() { f.serve(client, upstream) })
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Wait()
	})

	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.Fallbacks = nil
	cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	opts.DB = &db.DB{Pool: pool}
}

func (f *exportClaimCommitFault) waitGate() bool {
	select {
	case <-f.gate:
		return true
	case <-f.ctx.Done():
		return false
	}
}

func (f *exportClaimCommitFault) serve(client net.Conn, upstream string) {
	defer client.Close()
	startup, err := exportCommitPacket(client, true)
	// Suppress CancelRequest. Losing the application connection must not
	// cancel the independent server connection that still owns the claim.
	if err != nil || binary.BigEndian.Uint32(startup[4:8])>>16 != pgproto3.ProtocolVersion30>>16 {
		return
	}
	server, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(f.ctx, "tcp", upstream)
	if err != nil {
		return
	}
	var dropping atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer client.Close()
		var outcome string
		for {
			pkt, err := exportCommitPacket(server, false)
			if err != nil {
				return
			}
			if dropping.Load() {
				switch pkt[0] {
				case 'C': // CommandComplete
					outcome = string(bytes.TrimRight(pkt[5:], "\x00"))
				case 'Z': // ReadyForQuery
					f.outcome <- outcome
					if !f.early {
						f.waitGate()
					}
					return
				}
			} else if _, err := client.Write(pkt); err != nil {
				return
			}
		}
	}()
	defer func() {
		_ = server.Close()
		<-done
	}()
	if _, err := server.Write(startup); err != nil {
		return
	}
	claim := false
	for {
		pkt, err := exportCommitPacket(client, false)
		if err != nil {
			return
		}
		switch pkt[0] {
		case 'P': // Parse
			_, query, _ := strings.Cut(string(pkt[5:]), "\x00")
			if strings.HasPrefix(query, "UPDATE snapshots SET export_state =") && strings.Contains(query, "export_part_size_bytes =") {
				claim = true
			}
		case 'Q': // Query
			query := string(bytes.TrimRight(pkt[5:], "\x00"))
			if claim && query == "commit" && f.triggered.CompareAndSwap(false, true) {
				dropping.Store(true)
				if f.early {
					_ = client.Close()
					if !f.waitGate() {
						return
					}
				}
				if f.rollback {
					pkt, _ = (&pgproto3.Query{String: "rollback"}).Encode(nil)
				}
				if _, err := server.Write(pkt); err == nil {
					<-done
				}
				return
			}
			if query == "commit" || query == "rollback" {
				claim = false
			}
		}
		if _, err := server.Write(pkt); err != nil {
			return
		}
	}
}

func exportCommitPacket(r io.Reader, startup bool) ([]byte, error) {
	headerSize, sizeOffset := 5, 1
	if startup {
		headerSize, sizeOffset = 8, 0
	}
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint32(header[sizeOffset:])) + sizeOffset
	if size < headerSize || size > 16<<20 {
		return nil, fmt.Errorf("invalid PostgreSQL packet size: %d", size)
	}
	pkt := make([]byte, size)
	copy(pkt, header)
	_, err := io.ReadFull(r, pkt[headerSize:])
	return pkt, err
}

func (f *exportClaimCommitFault) wantOutcome(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-f.outcome:
		if got != want {
			t.Fatalf("PostgreSQL commit outcome = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy did not observe the export claim's transaction outcome")
	}
}

func TestExportClaimCommitRecovery(t *testing.T) {
	t.Parallel()
	const (
		notExported = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED
		exporting   = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING
		failed      = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED
	)
	for _, name := range []string{"committed", "rolled back", "still committing", "deleted", "settled by replacement", "newer attempt", "budget expires", "budget expires after replacement"} {
		t.Run(name, func(t *testing.T) {
			expires := strings.HasPrefix(name, "budget expires")
			early := name == "still committing" || expires
			fault := newExportClaimCommitFault(t, early, name == "rolled back")
			budget := 8 * time.Second
			if expires {
				budget = 2 * time.Second
			}
			env := testenv.New(t, exportFailTiming(budget, 20*time.Millisecond),
				func(o *app.Options) { fault.install(t, o) })
			org := env.NewOrg("alice")
			ctx := t.Context()
			c := org.OwnerClient
			dev, agent := setupAgent(t, env, org, nil)
			created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandbox := created.Msg.GetSandbox().GetSandboxId()
			waitState(t, c, org.ID, sandbox, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			snap := plainSnapshot(t, org, sandbox)
			original := env.App.Service.Hub.Live(dev.ID)
			if original == nil {
				t.Fatal("the original agent channel is missing")
			}
			holdExports(agent)
			result := make(chan error, 1)
			go func() {
				_, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snap}))
				result <- err
			}()
			var originalCommand, newerCommand string
			switch {
			case early:
				waitForLockWaiterCount(t, env, 1)
			case fault.rollback:
				fault.wantOutcome(t, "ROLLBACK")
			default:
				fault.wantOutcome(t, "COMMIT")
				var owner string
				originalCommand, owner = exportColumns(t, env, snap)
				if originalCommand == "" || owner != original.ID || snapshotExportState(t, c, org.ID, snap) != exporting {
					t.Fatalf("lost commit did not leave an EXPORTING claim for the original channel, command=%q owner=%q", originalCommand, owner)
				}
			}
			if ids := exportCommandIDs(agent); len(ids) != 0 {
				t.Fatalf("export commands before releasing the fault = %v", ids)
			}

			stay := original
			retryAgent := agent
			switch name {
			case "deleted":
				if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snap})); err != nil {
					t.Fatal(err)
				}
			case "settled by replacement", "newer attempt":
				reconnect(agent)
				waitExportState(t, c, org.ID, snap, failed)
				stay = env.App.Service.Hub.Live(dev.ID)
				if stay == nil || stay == original {
					t.Fatal("the replacement agent channel is missing")
				}
				if name == "newer attempt" {
					exportSnapshotRPC(t, c, org.ID, snap)
					newerCommand = waitExportCommands(t, agent, 1)[0]
				}
			case "budget expires after replacement":
				retryAgent = env.NewFakeAgent(dev)
				holdExports(retryAgent)
				retryAgent.SetSandbox(sandbox, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
				retryAgent.AddSnapshot(&ownsandv1.SnapshotStateEntry{SnapshotId: snap, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY})
				// The replacement registers before its state report waits
				// on the snapshot lock. Keep the claim pending until the
				// recovery budget ends on that new channel.
				retryAgent.OpenChannel()
				testenv.Eventually(t, "replacement channel registered", func() bool {
					stay = env.App.Service.Hub.Get(dev.ID)
					return stay != nil && stay != original
				})
			}
			if !expires {
				fault.release()
			}
			select {
			case err := <-result:
				if connect.CodeOf(err) != connect.CodeInternal {
					t.Fatalf("ExportSnapshot commit error = %v, want internal", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ExportSnapshot did not return its transaction error")
			}

			logs := env.Logs.String()
			if !strings.Contains(logs, `"reason":"claim_commit_failed"`) || !strings.Contains(logs, `"commit_error"`) {
				t.Fatal("no outcome was logged for the failed claim commit")
			}
			if expires {
				waitConnClosed(t, original)
				wantClosed := stay == original
				if !strings.Contains(logs, fmt.Sprintf(`"channel_closed":%t`, wantClosed)) {
					t.Fatalf("the recovery fallback did not report channel_closed=%t", wantClosed)
				}
				fault.release()
				fault.wantOutcome(t, "COMMIT")
				originalCommand, _ = exportColumns(t, env, snap)
				if wantClosed {
					if got := snapshotExportState(t, c, org.ID, snap); got != exporting {
						t.Fatalf("state after budget expiry = %s, want EXPORTING", got)
					}
					reconnect(agent)
				} else {
					connStaysOpen(t, stay)
				}
				waitExportState(t, c, org.ID, snap, failed)
			} else {
				connStaysOpen(t, stay)
			}
			if name == "still committing" {
				fault.wantOutcome(t, "COMMIT")
				originalCommand, _ = exportColumns(t, env, snap)
			}
			wantState := failed
			switch name {
			case "deleted":
				_, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snap}))
				if connect.CodeOf(err) != connect.CodeNotFound {
					t.Fatalf("GetSnapshot after deletion = %v, want not_found", err)
				}
				return
			case "rolled back":
				wantState = notExported
			case "newer attempt":
				wantState = exporting
			}
			if got := snapshotExportState(t, c, org.ID, snap); got != wantState {
				t.Fatalf("export state = %s, want %s", got, wantState)
			}
			if command, _ := exportColumns(t, env, snap); name == "newer attempt" {
				if command != newerCommand || command == originalCommand {
					t.Fatalf("the newer attempt was changed, command = %q, want %q", command, newerCommand)
				}
				return
			} else if command != originalCommand {
				t.Fatalf("stored command = %q, want %q", command, originalCommand)
			}
			if ids := exportCommandIDs(agent); len(ids) != 0 {
				t.Fatalf("export commands before explicit retry = %v", ids)
			}
			if ids := exportCommandIDs(retryAgent); len(ids) != 0 {
				t.Fatalf("retry channel already received export commands: %v", ids)
			}
			if name == "committed" || name == "rolled back" || name == "still committing" {
				if jobs := cleanupJobs(t, env); len(jobs) != 0 {
					t.Fatalf("the unsent attempt has clean-up jobs: %v", jobs)
				}
			}
			if got := exportSnapshotRPC(t, c, org.ID, snap); got != exporting {
				t.Fatalf("retry state = %s, want EXPORTING", got)
			}
			if retry := waitExportCommands(t, retryAgent, 1)[0]; retry == originalCommand {
				t.Fatalf("retry reused the unsent command ID %q", retry)
			}
		})
	}
}
