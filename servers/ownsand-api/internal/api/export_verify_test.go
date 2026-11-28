// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// verifyDelay is the wait between an export and its first R2 listing.
const verifyDelay = 15 * time.Minute
const verifyPoll = 30 * time.Second

// exportVerifyFixture is an exported snapshot of the fake agent: two parts of
// 1 MiB under the prefix of its attempt.
type exportVerifyFixture struct {
	env    *testenv.Env
	org    *testenv.Org
	snapID string
	ticks  chan<- time.Time
	// prefix is the attempt prefix; part1 and part2 are the keys of its parts.
	prefix, part1, part2 string
	// exportedAt is the clock reading at the end of the export.
	exportedAt time.Time
}

func newExportVerifyFixture(t *testing.T, options ...testenv.Option) *exportVerifyFixture {
	t.Helper()
	ticks := make(chan time.Time)
	env := testenv.New(t, append(options, func(opts *app.Options) {
		opts.ExportVerifyTicks = ticks
		opts.Timing.ExportVerifyPoll = verifyPoll
	})...)
	org := env.NewOrg("alice")
	setupAgent(t, env, org, nil)
	exportedAt := env.Clock.Now()
	snapID := exportedSnapshot(t, org)
	command, _ := exportColumns(t, env, snapID)
	prefix := attemptPrefixOf(org.ID, snapID, command)
	return &exportVerifyFixture{env: env, org: org, snapID: snapID, ticks: ticks, prefix: prefix, part1: prefix + "1", part2: prefix + "2", exportedAt: exportedAt}
}

// verification returns the export state and the verification columns.
func (f *exportVerifyFixture) verification(t *testing.T) (state ownsandv1.SnapshotExportState, verified bool, after *time.Time) {
	t.Helper()
	var export int16
	if err := f.env.DB.Pool.QueryRow(context.Background(), "SELECT export_state, export_verified, export_verify_after FROM snapshots WHERE snapshot_id = $1", f.snapID).
		Scan(&export, &verified, &after); err != nil {
		t.Fatal(err)
	}
	return ownsandv1.SnapshotExportState(export), verified, after
}

type exportManifestPart struct {
	PartNumber int32
	SizeBytes  int64
	SHA256     []byte
}

func (f *exportVerifyFixture) manifest(t *testing.T) []exportManifestPart {
	t.Helper()
	rows, err := f.env.DB.Pool.Query(context.Background(),
		"SELECT part_number, size_bytes, sha256 FROM snapshot_parts WHERE snapshot_id = $1 ORDER BY part_number", f.snapID)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[exportManifestPart])
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// run advances the clock by d and makes one verifier pass.
func (f *exportVerifyFixture) run(d time.Duration) {
	f.env.Clock.Advance(d)
	f.env.App.Control.RunExportVerifyOnce(context.Background())
}

func (f *exportVerifyFixture) wantExported(t *testing.T, verified bool) {
	t.Helper()
	if state, got, _ := f.verification(t); state != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || got != verified {
		t.Fatalf("export state %v, verified %v; want EXPORTED, verified %v", state, got, verified)
	}
}

func (f *exportVerifyFixture) wantLog(t *testing.T, parts ...string) {
	t.Helper()
	logs := f.env.Logs.String()
	for _, p := range parts {
		if !strings.Contains(logs, p) {
			t.Fatalf("logs lack %s", p)
		}
	}
}

func (f *exportVerifyFixture) wantLogCount(t *testing.T, event string, want int, fields ...string) {
	t.Helper()
	count := 0
	for line := range strings.SplitSeq(f.env.Logs.String(), "\n") {
		if !strings.Contains(line, `"msg":"`+event+`"`) {
			continue
		}
		count++
		for _, field := range fields {
			if !strings.Contains(line, field) {
				t.Errorf("%s event lacks %s: %s", event, field, line)
			}
		}
	}
	if count != want {
		t.Errorf("%s event count = %d, want %d", event, count, want)
	}
}

// wantFailed checks that the verification failed the export for reason: the
// snapshot is EXPORT_FAILED, its manifest is gone and a clean-up job covers
// the attempt prefix.
func (f *exportVerifyFixture) wantFailed(t *testing.T, reason, key string) {
	t.Helper()
	if state, _, _ := f.verification(t); state != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED {
		t.Fatalf("export state = %v, want EXPORT_FAILED", state)
	}
	if got := len(manifestHashes(t, f.env, f.snapID)); got != 0 {
		t.Fatalf("manifest has %d parts after the failure, want none", got)
	}
	if _, ok := cleanupJobs(t, f.env)[f.prefix]; !ok {
		t.Fatalf("no clean-up job for %s: %v", f.prefix, cleanupJobs(t, f.env))
	}
	f.wantLog(t, `"msg":"export_verification_failed"`, `"reason":"`+reason+`"`, `"object_key":"`+key+`"`, `"snapshot_id":"`+f.snapID+`"`)
}

func TestExportVerifierControlledTick(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	f.env.Clock.Advance(verifyDelay)
	f.wantExported(t, false)
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("list calls before tick = %d, want 0", got)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case f.ticks <- f.env.Clock.Now():
	case <-ctx.Done():
		t.Fatal("export verifier did not receive the tick")
	}

	waitLog(t, f.env, `"msg":"export_verified"`)
	f.wantExported(t, true)
	if got := f.env.Objects.ListCalls(); got != 1 {
		t.Fatalf("list calls after tick = %d, want 1", got)
	}
}

// TestExportVerificationPassesMatchingParts checks that the export becomes
// verified once, never lists R2 before export_verify_after and never lists a
// verified row again, even when an object is replaced later.
func TestExportVerificationPassesMatchingParts(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	if state, verified, after := f.verification(t); state != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || verified || after == nil || !after.Equal(f.exportedAt.Add(verifyDelay)) {
		t.Fatalf("after the export: state %v, verified %v, verify_after %v; want EXPORTED, unverified, %v", state, verified, after, f.exportedAt.Add(verifyDelay))
	}
	// Too early: one second before export_verify_after nothing is listed.
	f.run(verifyDelay - time.Second)
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("listed %d times before export_verify_after", got)
	}
	f.wantExported(t, false)

	f.run(time.Second)
	f.wantExported(t, true)
	if got := f.env.Objects.ListCalls(); got != 1 {
		t.Fatalf("list calls = %d, want 1", got)
	}
	f.wantLog(t, `"msg":"export_verified"`, `"snapshot_id":"`+f.snapID+`"`)

	// A verified row is never listed again, so a later replacement changes
	// nothing.
	f.env.Objects.PutSized(f.part1, 9<<20)
	f.run(time.Hour)
	f.wantExported(t, true)
	if got := f.env.Objects.ListCalls(); got != 1 {
		t.Fatalf("list calls = %d after verification, want 1", got)
	}
	if got := len(manifestHashes(t, f.env, f.snapID)); got != 2 {
		t.Fatalf("manifest has %d parts, want 2", got)
	}
}

// TestExportVerificationFailsMismatchingParts checks that a size difference,
// an unlisted object and a missing part each fail the export, delete the
// manifest, start the attempt prefix clean-up and allow a new export.
func TestExportVerificationFailsMismatchingParts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		part   string
		tamper func(f *exportVerifyFixture)
	}{
		{"size difference", "size_mismatch", "2", func(f *exportVerifyFixture) { f.env.Objects.PutSized(f.part2, 1<<20+1) }},
		{"late replacement", "size_mismatch", "1", func(f *exportVerifyFixture) { f.env.Objects.PutObject(f.part1, make([]byte, 7)) }},
		{"unlisted object", "unlisted_object", "3", func(f *exportVerifyFixture) { f.env.Objects.PutSized(f.prefix+"3", 1) }},
		{"missing part", "missing_part", "2", func(f *exportVerifyFixture) { f.env.Objects.RemoveObject(f.part2) }},
		{"wrong size then missing part", "missing_part", "2", func(f *exportVerifyFixture) {
			f.env.Objects.PutSized(f.part1, 7)
			f.env.Objects.RemoveObject(f.part2)
		}},
		{"missing part then wrong size", "missing_part", "1", func(f *exportVerifyFixture) {
			f.env.Objects.RemoveObject(f.part1)
			f.env.Objects.PutSized(f.part2, 7)
		}},
		{"wrong size and unlisted object", "size_mismatch", "1", func(f *exportVerifyFixture) {
			f.env.Objects.PutSized(f.part1, 7)
			f.env.Objects.PutSized(f.prefix+"3", 1)
		}},
		{"missing part and unlisted object", "missing_part", "2", func(f *exportVerifyFixture) {
			f.env.Objects.RemoveObject(f.part2)
			f.env.Objects.PutSized(f.prefix+"3", 1)
		}},
		{"two missing parts", "missing_part", "1", func(f *exportVerifyFixture) {
			f.env.Objects.RemoveObject(f.part1)
			f.env.Objects.RemoveObject(f.part2)
		}},
		{"two size differences", "size_mismatch", "1", func(f *exportVerifyFixture) {
			f.env.Objects.PutSized(f.part1, 7)
			f.env.Objects.PutSized(f.part2, 7)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportVerifyFixture(t)
			tc.tamper(f)
			f.run(verifyDelay)
			f.wantFailed(t, tc.reason, f.prefix+tc.part)

			// The clean-up job deletes the attempt prefix, and a new export
			// starts a new verification.
			f.env.App.Control.RunCleanupOnce(context.Background())
			if got := f.env.Objects.DeletedPrefixes(); len(got) != 1 || got[0] != f.prefix {
				t.Fatalf("clean-up sweeps = %v, want %s", got, f.prefix)
			}
			if got := exportSnapshotRPC(t, f.org.OwnerClient, f.org.ID, f.snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
				t.Fatalf("retry state = %v", got)
			}
			waitExportState(t, f.org.OwnerClient, f.org.ID, f.snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
			f.wantExported(t, false)
			f.run(verifyDelay)
			f.wantExported(t, true)
		})
	}
}

// TestExportVerificationRetriesAfterR2Failure checks that an R2 listing error
// preserves the export and that a later pass verifies it.
func TestExportVerificationRetriesAfterR2Failure(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	f.env.Objects.SetListFail(errors.New("r2 is down"))
	f.run(verifyDelay)
	f.wantExported(t, false)
	if got := len(manifestHashes(t, f.env, f.snapID)); got != 2 {
		t.Fatalf("manifest has %d parts after an R2 failure, want 2", got)
	}
	if got := len(cleanupJobs(t, f.env)); got != 0 {
		t.Fatalf("%d clean-up jobs after an R2 failure, want none", got)
	}
	f.wantLog(t, `"msg":"export_verification_retry"`, `"reason":"r2_list_failed"`)

	f.env.Objects.SetListFail(nil)
	f.run(verifyPoll - time.Second)
	f.wantExported(t, false)
	if got := f.env.Objects.ListCalls(); got != 1 {
		t.Fatalf("list calls before retry = %d, want 1", got)
	}
	f.run(time.Second)
	f.wantExported(t, true)
}

// TestExportVerificationWithoutObjectStore checks that a control plane
// without R2 changes nothing.
func TestExportVerificationWithoutObjectStore(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	manifest := f.manifest(t)
	if len(manifest) != 2 {
		t.Fatalf("initial manifest has %d parts, want 2", len(manifest))
	}
	_, _, deadline := f.verification(t)
	if want := f.exportedAt.Add(verifyDelay); deadline == nil || !deadline.Equal(want) {
		t.Fatalf("initial verification deadline = %v, want %v", deadline, want)
	}
	if got := len(cleanupJobs(t, f.env)); got != 0 {
		t.Fatalf("initial cleanup queue has %d jobs, want none", got)
	}

	f.env.App.Service.Objects = nil
	for i, delay := range []time.Duration{verifyDelay, time.Hour} {
		f.run(delay)
		state, verified, after := f.verification(t)
		if state != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || verified {
			t.Fatalf("pass %d: export state %v, verified %v; want EXPORTED, unverified", i+1, state, verified)
		}
		if after == nil || !after.Equal(*deadline) {
			t.Fatalf("pass %d: verification deadline = %v, want %v", i+1, after, deadline)
		}
		if got := f.manifest(t); !slices.EqualFunc(got, manifest, func(a, b exportManifestPart) bool {
			return a.PartNumber == b.PartNumber && a.SizeBytes == b.SizeBytes && bytes.Equal(a.SHA256, b.SHA256)
		}) {
			t.Fatalf("pass %d: manifest = %+v, want %+v", i+1, got, manifest)
		}
		if jobs := cleanupJobs(t, f.env); len(jobs) != 0 {
			t.Fatalf("pass %d: cleanup queue = %v, want empty", i+1, jobs)
		}
	}
	f.wantLog(t, `"msg":"export_verification_retry"`, `"reason":"r2_not_configured"`)
}

// TestExportVerificationSchedulesRowsWithoutTime checks that a row without
// export_verify_after (exported before the migration, or by a release that
// does not set it) is first scheduled for the upload URL lifetime after it is
// seen, and verified only then.
func TestExportVerificationSchedulesRowsWithoutTime(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	if _, err := f.env.DB.Pool.Exec(context.Background(), "UPDATE snapshots SET export_verified = false, export_verify_after = NULL WHERE snapshot_id = $1", f.snapID); err != nil {
		t.Fatal(err)
	}
	// Long after the export, the first sight only schedules the row.
	f.run(24 * time.Hour)
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("listed %d times at the first sight of the row", got)
	}
	seen := f.env.Clock.Now()
	if _, verified, after := f.verification(t); verified || after == nil || !after.Equal(seen.Add(verifyDelay)) {
		t.Fatalf("verified %v, verify_after %v; want unverified and %v", verified, after, seen.Add(verifyDelay))
	}
	f.run(verifyDelay - time.Second)
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("listed %d times before the scheduled time", got)
	}
	f.run(time.Second)
	f.wantExported(t, true)
	if got := f.env.Objects.ListCalls(); got != 1 {
		t.Fatalf("list calls = %d, want 1", got)
	}
}

type exportVerifyStore struct {
	objectstore.Store
	list func(context.Context, string) ([]objectstore.Object, error)
}

func (s exportVerifyStore) ListPrefix(ctx context.Context, prefix string) ([]objectstore.Object, error) {
	return s.list(ctx, prefix)
}

func (f *exportVerifyFixture) rejectVerificationUpdate(t *testing.T, trigger string) {
	t.Helper()
	for _, sql := range []string{
		`CREATE FUNCTION reject_verification_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected verification update failure';
		END $$`,
		trigger,
	} {
		if _, err := f.env.DB.Pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *exportVerifyFixture) allowVerificationUpdate(t *testing.T) {
	t.Helper()
	if _, err := f.env.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_verification_update ON snapshots"); err != nil {
		t.Fatal(err)
	}
}

func TestExportVerificationScheduleCommitFailure(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	if _, err := f.env.DB.Pool.Exec(t.Context(), "UPDATE snapshots SET export_verify_after = NULL WHERE snapshot_id = $1", f.snapID); err != nil {
		t.Fatal(err)
	}
	f.rejectVerificationUpdate(t, `
		CREATE CONSTRAINT TRIGGER reject_verification_update AFTER UPDATE ON snapshots
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		WHEN (OLD.export_verify_after IS NULL AND NEW.export_verify_after IS NOT NULL)
		EXECUTE FUNCTION reject_verification_update()`)
	control, requests := r2RetryControl(t, f.env, f.prefix, nil)
	// PostgreSQL stores the deadline with microsecond precision.
	f.env.Clock.Advance(verifyDelay + time.Nanosecond)
	control.RunExportVerifyOnce(t.Context())
	f.wantExported(t, false)
	if _, _, after := f.verification(t); after != nil {
		t.Fatalf("rolled-back deadline = %v, want NULL", after)
	}
	f.wantLogCount(t, "export_verification_scan_failed", 1, "injected verification update failure")
	f.wantLogCount(t, "export_verification_scheduled", 0)

	f.allowVerificationUpdate(t)
	control.RunExportVerifyOnce(t.Context())
	control.RunExportVerifyOnce(t.Context())
	f.wantExported(t, false)
	_, _, after := f.verification(t)
	if want := f.env.Clock.Now().Add(verifyDelay).Truncate(time.Microsecond); after == nil || !after.Equal(want) {
		t.Fatalf("committed deadline = %v, want %v", after, want)
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("R2 requests while scheduling = %q, want none", got)
	}
	f.wantLogCount(t, "export_verification_scheduled", 1, `"level":"INFO"`,
		`"snapshot_id":"`+f.snapID+`"`, `"verify_after":"`+after.UTC().Format(time.RFC3339Nano)+`"`)
}

func TestExportVerificationScheduleBatchRollback(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t)
	due := f.seedOlderExportVerifications(t, 1)[0]
	_, _, originalDue := due.verification(t)
	if _, err := f.env.DB.Pool.Exec(t.Context(), "UPDATE snapshots SET export_verify_after = NULL WHERE snapshot_id = $1", f.snapID); err != nil {
		t.Fatal(err)
	}
	// The unset deadline sorts first. Reject only the later row's update.
	f.rejectVerificationUpdate(t, `
		CREATE TRIGGER reject_verification_update BEFORE UPDATE OF export_verify_after ON snapshots
		FOR EACH ROW WHEN (OLD.created_by_id = 'verification-fault')
		EXECUTE FUNCTION reject_verification_update()`)
	f.run(verifyDelay)
	if _, _, after := f.verification(t); after != nil {
		t.Fatalf("rolled-back first deadline = %v, want NULL", after)
	}
	if _, _, after := due.verification(t); after == nil || !after.Equal(*originalDue) {
		t.Fatalf("later deadline = %v, want %v", after, originalDue)
	}
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("R2 listings after failed claim = %d, want 0", got)
	}
	f.wantLogCount(t, "export_verification_scan_failed", 1, "injected verification update failure")
	f.wantLogCount(t, "export_verification_scheduled", 0)
}

func TestExportVerificationApplyCommitFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		missingPart bool
	}{
		{name: "matching listing"},
		{name: "missing part", missingPart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportVerifyFixture(t)
			command, _ := exportColumns(t, f.env, f.snapID)
			snapshotField := `"snapshot_id":"` + f.snapID + `"`
			commandField := `"command_id":"` + command + `"`
			before := manifestHashes(t, f.env, f.snapID)
			f.rejectVerificationUpdate(t, `
				CREATE CONSTRAINT TRIGGER reject_verification_update AFTER UPDATE ON snapshots
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
				WHEN (OLD.export_state IS DISTINCT FROM NEW.export_state OR OLD.export_verified IS DISTINCT FROM NEW.export_verified)
				EXECUTE FUNCTION reject_verification_update()`)
			const partXML = "<Contents><Key>%s</Key><Size>1048576</Size></Contents>"
			contents := fmt.Sprintf(partXML, f.part1)
			if !tc.missingPart {
				contents += fmt.Sprintf(partXML, f.part2)
			}
			listing := "<ListBucketResult>" + contents + "</ListBucketResult>"
			pages := []r2ListingPage{{body: listing}, {body: listing}}
			control, requests := r2RetryControl(t, f.env, f.prefix, pages)
			f.env.Clock.Advance(verifyDelay)
			control.RunExportVerifyOnce(t.Context())
			f.wantExported(t, false)
			_, _, after := f.verification(t)
			if want := f.env.Clock.Now().Add(verifyPoll); after == nil || !after.Equal(want) {
				t.Fatalf("retry deadline = %v, want %v", after, want)
			}
			if got := manifestHashes(t, f.env, f.snapID); !slices.EqualFunc(got, before, bytes.Equal) {
				t.Fatal("failed commit changed the manifest")
			}
			if got := cleanupJobs(t, f.env); len(got) != 0 {
				t.Fatalf("failed commit queued clean-up: %v", got)
			}
			f.wantLogCount(t, "export_verification_retry", 1, snapshotField, commandField, `"reason":"database_error"`, "injected verification update failure")
			f.wantLogCount(t, "export_verified", 0)
			f.wantLogCount(t, "export_verification_failed", 0)

			f.allowVerificationUpdate(t)
			f.env.Clock.Advance(verifyPoll)
			control.RunExportVerifyOnce(t.Context())
			if got := requests(); !slices.Equal(got, []string{"list ", "list "}) {
				t.Fatalf("R2 requests across failed apply and retry = %q, want two listings", got)
			}
			if tc.missingPart {
				f.wantFailed(t, "missing_part", f.part2)
				f.wantLogCount(t, "export_verification_failed", 1, `"level":"WARN"`, snapshotField, commandField,
					`"reason":"missing_part"`, `"object_key":"`+f.part2+`"`)
				f.wantLogCount(t, "export_verified", 0)
			} else {
				f.wantExported(t, true)
				f.wantLogCount(t, "export_verified", 1, `"level":"INFO"`, snapshotField, commandField, `"parts":2`)
				f.wantLogCount(t, "export_verification_failed", 0)
			}
		})
	}
}

// seedOlderExportVerifications adds due exports ahead of f's export. Each
// has a distinct attempt and a one-part manifest that matches the store.
func (f *exportVerifyFixture) seedOlderExportVerifications(t *testing.T, count int) []*exportVerifyFixture {
	t.Helper()
	rows, err := f.env.DB.Pool.Query(context.Background(), `
		WITH older AS (
			INSERT INTO snapshots (
				snapshot_id, organization_id, source_sandbox_id, state, export_state, guest_os,
				host_os, arch, vcpu_count, memory_mib, disk_gib, network_mode, created_by_kind,
				created_by_id, create_time, export_command_id, export_verify_after
			)
			SELECT gen_random_uuid(), organization_id, source_sandbox_id, state, export_state,
				guest_os, host_os, arch, vcpu_count, memory_mib, disk_gib, network_mode,
				created_by_kind, 'verification-fault', create_time, gen_random_uuid(),
				export_verify_after - n * interval '1 second'
			FROM snapshots CROSS JOIN generate_series(1, $2::integer) AS n
			WHERE snapshot_id = $1
			RETURNING snapshot_id, export_command_id
		), parts AS (
			INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256)
			SELECT snapshot_id, 1, 1, decode(repeat('00', 32), 'hex') FROM older
			RETURNING snapshot_id
		)
		SELECT snapshot_id::text, export_command_id::text FROM older JOIN parts USING (snapshot_id)`, f.snapID, count)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var older []*exportVerifyFixture
	for rows.Next() {
		var id, command string
		if err := rows.Scan(&id, &command); err != nil {
			t.Fatal(err)
		}
		prefix := attemptPrefixOf(f.org.ID, id, command)
		f.env.Objects.PutSized(prefix+"1", 1)
		older = append(older, &exportVerifyFixture{env: f.env, snapID: id})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(older) != count {
		t.Fatalf("seeded %d older exports, want %d", len(older), count)
	}
	return older
}

func TestExportVerificationRotatesRetries(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"r2_list_failed", "database_error"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			f := newExportVerifyFixture(t)
			older := f.seedOlderExportVerifications(t, 50)
			faults := true
			var listed []string
			f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, prefix string) ([]objectstore.Object, error) {
				listed = append(listed, prefix)
				if reason == "r2_list_failed" && faults && prefix != f.prefix {
					return nil, errors.New("injected listing error")
				}
				return f.env.Objects.ListPrefix(ctx, prefix)
			}}
			if reason == "database_error" {
				// Reject the verification write for older rows after the
				// claim has committed and listing has succeeded.
				_, err := f.env.DB.Pool.Exec(ctx, `
					CREATE FUNCTION reject_export_verification() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						RAISE EXCEPTION 'injected apply error';
					END;
					$$;
					CREATE TRIGGER reject_export_verification
					BEFORE UPDATE OF export_verified ON snapshots FOR EACH ROW
					WHEN (NEW.export_verified AND OLD.created_by_id = 'verification-fault')
					EXECUTE FUNCTION reject_export_verification()`)
				if err != nil {
					t.Fatal(err)
				}
			}

			f.run(verifyDelay)
			if len(listed) != 50 {
				t.Fatalf("first pass listed %d prefixes, want 50", len(listed))
			}
			f.wantExported(t, false)
			firstRetry := f.env.Clock.Now().Add(verifyPoll)
			for _, job := range older {
				_, _, after := job.verification(t)
				if after == nil || !after.Equal(firstRetry) {
					t.Fatalf("first retry for %s = %v, want %v", job.snapID, after, firstRetry)
				}
			}

			listed = nil
			f.run(verifyPoll)
			if len(listed) != 50 || listed[0] != f.prefix {
				t.Fatalf("second pass listed %d prefixes, want 50 starting with %s", len(listed), f.prefix)
			}
			f.wantExported(t, true)
			for _, job := range older {
				job.wantExported(t, false)
				if got := len(manifestHashes(t, f.env, job.snapID)); got != 1 {
					t.Fatalf("manifest for %s has %d parts after failure, want 1", job.snapID, got)
				}
			}
			if got := len(cleanupJobs(t, f.env)); got != 0 {
				t.Fatalf("%d clean-up jobs after failures, want none", got)
			}
			f.wantLog(t, `"msg":"export_verification_retry"`, `"reason":"`+reason+`"`)

			faults = false
			if reason == "database_error" {
				if _, err := f.env.DB.Pool.Exec(ctx, "DROP TRIGGER reject_export_verification ON snapshots"); err != nil {
					t.Fatal(err)
				}
			}
			f.run(verifyPoll)
			for _, job := range older {
				job.wantExported(t, true)
			}
		})
	}
}

func TestExportVerificationIgnoresStaleWork(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"replace after listing error", "replace before apply", "verify before apply", "delete before apply"} {
		t.Run(action, func(t *testing.T) {
			f := newExportVerifyFixture(t)
			replacementDeadline := f.exportedAt.Add(2 * verifyDelay)
			var calls int
			f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, prefix string) ([]objectstore.Object, error) {
				calls++
				if prefix != f.prefix {
					t.Fatalf("listed %s, want %s", prefix, f.prefix)
				}
				pool := f.env.DB.Pool
				var err error
				switch action {
				case "replace after listing error", "replace before apply":
					_, err = pool.Exec(ctx, "UPDATE snapshots SET export_command_id = gen_random_uuid(), export_verify_after = $2 WHERE snapshot_id = $1", f.snapID, replacementDeadline)
				case "verify before apply":
					_, err = pool.Exec(ctx, "UPDATE snapshots SET export_verified = true WHERE snapshot_id = $1", f.snapID)
				case "delete before apply":
					_, err = pool.Exec(ctx, "DELETE FROM snapshots WHERE snapshot_id = $1", f.snapID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if action == "replace after listing error" {
					return nil, errors.New("injected listing error")
				}
				// A missing part would fail the export if the result applied.
				return nil, nil
			}}

			f.run(verifyDelay)
			if calls != 1 {
				t.Fatalf("list calls = %d, want 1", calls)
			}
			switch action {
			case "replace after listing error", "replace before apply":
				f.wantExported(t, false)
				_, _, after := f.verification(t)
				if after == nil || !after.Equal(replacementDeadline) {
					t.Fatalf("replacement deadline = %v, want %v", after, replacementDeadline)
				}
				f.run(verifyDelay - time.Second)
				if calls != 1 {
					t.Fatalf("list calls before the replacement deadline = %d, want 1", calls)
				}
			case "verify before apply":
				f.wantExported(t, true)
			case "delete before apply":
				var exists bool
				if err := f.env.DB.Pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM snapshots WHERE snapshot_id = $1)", f.snapID).Scan(&exists); err != nil || exists {
					t.Fatalf("snapshot exists = %v after deletion, query error = %v", exists, err)
				}
			}
			if got := len(cleanupJobs(t, f.env)); got != 0 {
				t.Fatalf("stale work created %d clean-up jobs, want none", got)
			}
			f.wantLogCount(t, "export_verified", 0)
			f.wantLogCount(t, "export_verification_failed", 0)
		})
	}
}

func startExportVerifyPass(t *testing.T, f *exportVerifyFixture) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.env.App.Control.RunExportVerifyOnce(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		waitExportVerifySignal(t, done, "verifier pass to exit during cleanup")
	})
	return done
}

func waitExportVerifySignal(t *testing.T, signal <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
	}
}

func waitExportVerifyLock(t *testing.T, f *exportVerifyFixture, query string) int {
	t.Helper()
	var pid int
	testenv.Eventually(t, "verification query to wait on a lock", func() bool {
		err := f.env.DB.Pool.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, query).Scan(&pid)
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

func TestExportVerificationHandoverCancelsListing(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t, func(o *app.Options) {
		o.LeaseRenewTicks = make(chan time.Time)
	})
	peer := f.env.StartPeer()
	older := f.seedOlderExportVerifications(t, 1)[0]
	before := manifestHashes(t, f.env, older.snapID)
	started, canceled, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseListing := sync.OnceFunc(func() { close(resume) })
	defer releaseListing()
	var listed []string
	f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, prefix string) ([]objectstore.Object, error) {
		listed = append(listed, prefix)
		if len(listed) == 1 {
			context.AfterFunc(ctx, func() { close(canceled) })
			close(started)
			<-resume
		}
		// Simulate a successful response that arrives despite cancellation.
		return nil, nil
	}}
	f.env.Clock.Advance(verifyDelay)
	done := startExportVerifyPass(t, f)
	waitExportVerifySignal(t, started, "the first holder to list")
	f.env.App.Lease.Release()
	runHandoverLease(t, peer)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Error("takeover did not cancel the listing while the old service was running")
	}
	if err := t.Context().Err(); err != nil {
		t.Fatalf("test context ended during lease handover: %v", err)
	}
	releaseListing()
	waitExportVerifySignal(t, done, "the old pass to exit")
	if len(listed) != 1 || listed[0] == f.prefix {
		t.Errorf("old holder listings = %v, want only the older export", listed)
	}
	older.wantExported(t, false)
	f.wantExported(t, false)
	if got := manifestHashes(t, f.env, older.snapID); !slices.EqualFunc(got, before, bytes.Equal) {
		t.Fatal("late result changed the older export's manifest")
	}
	if got := len(f.manifest(t)); got != 2 {
		t.Fatalf("later export manifest has %d parts, want 2", got)
	}
	if got := cleanupJobs(t, f.env); len(got) != 0 {
		t.Fatalf("old pass queued cleanup: %v", got)
	}
	t.Log("takeover canceled the pending listing, discarded the late mismatch and stopped before the second listing")

	f.env.Clock.Advance(verifyPoll - time.Second)
	peer.App.Control.RunExportVerifyOnce(t.Context())
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("successor listed %d prefixes before their retry deadlines", got)
	}
	f.env.Clock.Advance(time.Second)
	peer.App.Control.RunExportVerifyOnce(t.Context())
	older.wantExported(t, true)
	f.wantExported(t, true)
	if got := f.env.Objects.ListCalls(); got != 2 {
		t.Fatalf("successor listed %d prefixes, want 2", got)
	}
	t.Log("the successor verified both exports at the existing retry deadline")
}

func TestExportVerificationLeaseReacquisitionFencesApply(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Time, 1)
	f := newExportVerifyFixture(t, func(o *app.Options) { o.LeaseRenewTicks = ticks })
	oldEpoch, _ := f.env.App.Lease.Ownership()
	before := manifestHashes(t, f.env, f.snapID)
	// Acquire the snapshot lock during the listing, after the claim has
	// released its locks, and keep it until after same-instance reacquisition.
	listed := make(chan error, 1)
	tx, err := f.env.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, _ string) ([]objectstore.Object, error) {
		_, err := tx.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", f.snapID)
		listed <- err
		return nil, err
	}}
	f.env.Clock.Advance(verifyDelay)
	done := startExportVerifyPass(t, f)
	select {
	case err := <-listed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the verifier did not list the export")
	}
	pid := waitExportVerifyLock(t, f, "%SELECT export_state,%FROM snapshots%FOR UPDATE%")
	f.env.App.Lease.Release()
	ticks <- time.Now()
	f.env.WaitForLease()
	if epoch, held := f.env.App.Lease.Ownership(); !held || epoch == oldEpoch {
		t.Fatal("original holder did not acquire a new lease period")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitExportVerifySignal(t, done, "the old apply to exit")
	f.wantExported(t, false)
	if got := manifestHashes(t, f.env, f.snapID); !slices.EqualFunc(got, before, bytes.Equal) {
		t.Fatal("outdated apply changed the manifest")
	}
	if jobs := cleanupJobs(t, f.env); len(jobs) != 0 {
		t.Fatalf("outdated apply queued cleanup: %v", jobs)
	}
	// The canceled connection can still hold the row until PostgreSQL
	// finishes the transaction. A claim skips locked snapshots.
	testenv.Eventually(t, "canceled apply transaction to end", func() bool {
		var ended bool
		err := f.env.DB.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND xact_start IS NOT NULL)`, pid).Scan(&ended)
		if err != nil {
			t.Fatal(err)
		}
		return ended
	})
	f.env.App.Service.Objects = f.env.Objects
	f.run(verifyPoll)
	f.wantExported(t, true)
	t.Log("same-instance reacquisition fenced the blocked apply and allowed a matching retry")
}

func TestExportVerificationLeaseTakeoverFencesClaim(t *testing.T) {
	t.Parallel()
	f := newExportVerifyFixture(t, func(o *app.Options) {
		o.LeaseRenewTicks = make(chan time.Time)
	})
	peer := f.env.StartPeer()
	_, _, original := f.verification(t)
	tx, err := f.env.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	// Claims skip locked rows, so use a table lock to pause the statement
	// after the pass has captured its period and before it can lock rows.
	if _, err := tx.Exec(t.Context(), "LOCK TABLE snapshots IN EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	f.env.Clock.Advance(verifyDelay)
	done := startExportVerifyPass(t, f)
	waitExportVerifyLock(t, f, "%FROM snapshots WHERE export_state%FOR UPDATE SKIP LOCKED%")
	f.env.App.Lease.Release()
	runHandoverLease(t, peer)
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitExportVerifySignal(t, done, "the old claim to exit")
	f.wantExported(t, false)
	if _, _, after := f.verification(t); after == nil || !after.Equal(*original) {
		t.Fatalf("old claim deadline = %v, want %v", after, original)
	}
	if got := f.env.Objects.ListCalls(); got != 0 {
		t.Fatalf("old claim made %d listings", got)
	}

	peer.App.Service.Objects = exportVerifyStore{Store: peer.Objects, list: func(context.Context, string) ([]objectstore.Object, error) {
		return []objectstore.Object{{Key: f.part1, Size: 1 << 20}}, nil
	}}
	peer.App.Control.RunExportVerifyOnce(t.Context())
	f.wantFailed(t, "missing_part", f.part2)
	t.Log("the old pass left the deadline intact and the successor applied a missing-part result")
}

func TestExportVerificationLeaseDatabaseExpiryFencesWrites(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"claim", "apply"} {
		t.Run(phase, func(t *testing.T) {
			f := newExportVerifyFixture(t, func(o *app.Options) {
				o.LeaseRenewTicks = make(chan time.Time)
			})
			_, _, original := f.verification(t)
			tx, err := f.env.DB.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			lock := func(ctx context.Context) error {
				_, err := tx.Exec(ctx, "SELECT 1 FROM instance_leases WHERE lease_name = $1 FOR UPDATE", lease.Name)
				return err
			}
			if phase == "claim" {
				if err := lock(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, _ string) ([]objectstore.Object, error) {
					return nil, lock(ctx)
				}}
			}
			f.env.Clock.Advance(verifyDelay)
			done := startExportVerifyPass(t, f)
			waitExportVerifyLock(t, f, "%FROM instance_leases%FOR SHARE%")
			// Expire the shared row while the local period still reports held.
			if _, err := tx.Exec(t.Context(), "UPDATE instance_leases SET expire_time = clock_timestamp() WHERE lease_name = $1", lease.Name); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitExportVerifySignal(t, done, "the attempt with an expired database lease to exit")
			if !f.env.App.Lease.Held() {
				t.Fatal("local lease ended before the write guard could be observed")
			}
			f.wantExported(t, false)
			if phase == "claim" {
				if _, _, after := f.verification(t); after == nil || !after.Equal(*original) {
					t.Fatalf("expired claim deadline = %v, want %v", after, original)
				}
				if got := f.env.Objects.ListCalls(); got != 0 {
					t.Fatalf("expired claim made %d listings", got)
				}
			}
			if got := len(f.manifest(t)); got != 2 {
				t.Fatalf("manifest has %d parts after lease expiry, want 2", got)
			}
			if jobs := cleanupJobs(t, f.env); len(jobs) != 0 {
				t.Fatalf("expired write queued cleanup: %v", jobs)
			}
			t.Log("the database clock rejected the write while the local lease still appeared current")
		})
	}
}

func TestExportVerificationLeaseRenewalKeepsListing(t *testing.T) {
	t.Parallel()
	clock := testenv.NewClock()
	ticks := make(chan time.Time, 1)
	f := newExportVerifyFixture(t, func(o *app.Options) {
		o.LeaseDeadlineNow = clock.Now
		o.LeaseRenewTicks = ticks
	})
	// Start the listing with only 10 seconds left in the initial period.
	clock.Advance(20 * time.Second)
	started := make(chan context.Context, 1)
	resume := make(chan struct{})
	releaseListing := sync.OnceFunc(func() { close(resume) })
	defer releaseListing()
	f.env.App.Service.Objects = exportVerifyStore{Store: f.env.Objects, list: func(ctx context.Context, prefix string) ([]objectstore.Object, error) {
		started <- ctx
		<-resume
		return f.env.Objects.ListPrefix(ctx, prefix)
	}}
	f.env.Clock.Advance(verifyDelay)
	done := startExportVerifyPass(t, f)
	var listCtx context.Context
	select {
	case listCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the verifier did not start a listing")
	}
	if deadline, ok := listCtx.Deadline(); !ok || time.Until(deadline) < 45*time.Second {
		t.Fatalf("listing deadline = %v, want the listing timeout independent of the first lease expiry", deadline)
	}
	owner := f.env.App.Lease
	var period *lease.Period
	if !owner.WhileHeld(func(p *lease.Period) { period = p }) {
		t.Fatal("lease ended before renewal")
	}
	ticks <- time.Now()
	testenv.Eventually(t, "the initial lease period to renew", func() bool {
		ctx, cancel := period.Context(t.Context())
		defer cancel()
		deadline, _ := ctx.Deadline()
		return time.Until(deadline) > 20*time.Second
	})
	clock.Advance(11 * time.Second)
	if listCtx.Err() != nil || !owner.Held() {
		t.Fatalf("renewal did not keep the original listing active: %v", listCtx.Err())
	}
	releaseListing()
	waitExportVerifySignal(t, done, "the renewed pass to exit")
	f.wantExported(t, true)
	t.Log("the listing stayed active past the initial lease expiry and its matching result committed")
}
