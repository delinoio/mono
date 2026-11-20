// SPDX-License-Identifier: FSL-1.1-ALv2

package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/pgtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

var owner = store.Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: "owner-sub"}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*store.Store, *clock) {
	st := pgtest.New(t)
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	st.Now = c.now
	return st, c
}

func newOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	org, err := st.CreateOrganization(context.Background(), "org", owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return org.ID
}

func newKey(t *testing.T) []byte {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func newToken(t *testing.T, st *store.Store, org string, ttl time.Duration) (string, store.EnrollmentToken) {
	t.Helper()
	v := secret.New(secret.KindEnrollmentToken)
	tok, err := st.CreateEnrollmentToken(context.Background(), org, "dev", ttl, owner, secret.Hash(v), secret.DisplayPrefix(v), "")
	if err != nil {
		t.Fatal(err)
	}
	return v, tok
}

func TestMigrateIsIdempotent(t *testing.T) {
	st, _ := newStore(t)
	if err := store.Migrate(context.Background(), st.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentEdgeCases(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	org := newOrg(t, st)
	caps := &ownsandv1.DeviceCapabilities{HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0"}

	tokenValue, _ := newToken(t, st, org, time.Hour)
	key := newKey(t)
	dev, gotOrg, err := st.Enroll(ctx, secret.Hash(tokenValue), key, caps, "")
	if err != nil || gotOrg != org {
		t.Fatalf("enroll: %v", err)
	}
	// A retry with the same token and key returns the same device.
	again, _, err := st.Enroll(ctx, secret.Hash(tokenValue), key, caps, "")
	if err != nil || again != dev {
		t.Fatalf("retry: %s %v", again, err)
	}
	// A retry with another key fails like a used token.
	if _, _, err := st.Enroll(ctx, secret.Hash(tokenValue), newKey(t), caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("reuse with another key: %v", err)
	}
	d, err := st.GetDevice(ctx, org, dev)
	if err != nil || d.State != ownsandv1.DeviceState_DEVICE_STATE_OFFLINE || d.Limits != (store.Limits{}) {
		t.Fatalf("new device: %+v %v", d, err)
	}

	// Revoked token.
	revokedValue, revoked := newToken(t, st, org, time.Hour)
	if err := st.RevokeEnrollmentToken(ctx, org, revoked.ID, owner, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Enroll(ctx, secret.Hash(revokedValue), newKey(t), caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("revoked: %v", err)
	}
	// Revoking a used token fails.
	_, used := newToken(t, st, org, time.Hour)
	_ = used
	toks, _ := st.ListEnrollmentTokens(ctx, org, store.Page{Size: 50})
	for _, tk := range toks {
		if tk.DeviceID == dev {
			if err := st.RevokeEnrollmentToken(ctx, org, tk.ID, owner, ""); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("revoke used: %v", err)
			}
		}
	}

	// Expired token, and a used token's retry after its expiry.
	expiredValue, _ := newToken(t, st, org, time.Minute)
	c.t = c.t.Add(2 * time.Hour)
	if _, _, err := st.Enroll(ctx, secret.Hash(expiredValue), newKey(t), caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("expired: %v", err)
	}
	if _, _, err := st.Enroll(ctx, secret.Hash(tokenValue), key, caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("retry after expiry: %v", err)
	}
	// Unknown token.
	if _, _, err := st.Enroll(ctx, secret.Hash(secret.New(secret.KindEnrollmentToken)), key, caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("unknown: %v", err)
	}
	// Token of a deleted organization.
	c.t = c.t.Add(-2 * time.Hour)
	lateValue, _ := newToken(t, st, org, time.Hour)
	if err := st.DeleteOrganization(ctx, org, owner, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Enroll(ctx, secret.Hash(lateValue), newKey(t), caps, ""); !errors.Is(err, store.ErrEnrollmentRejected) {
		t.Fatalf("deleted organization: %v", err)
	}
}

func enroll(t *testing.T, st *store.Store, org string) string {
	t.Helper()
	v, _ := newToken(t, st, org, time.Hour)
	dev, _, err := st.Enroll(context.Background(), secret.Hash(v), newKey(t), &ownsandv1.DeviceCapabilities{}, "")
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func TestDeviceRemovalAndTombstone(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	org := newOrg(t, st)
	dev := enroll(t, st, org)
	cred := secret.New(secret.KindDeviceSession)
	if err := st.CreateDeviceSession(ctx, dev, secret.Hash(cred), c.t.Add(15*time.Minute), "epoch-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupDeviceSession(ctx, secret.Hash(cred)); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveDevice(ctx, org, dev, owner, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupDeviceSession(ctx, secret.Hash(cred)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("session after removal: %v", err)
	}
	if err := st.CreateDeviceSession(ctx, dev, secret.Hash(secret.New(secret.KindDeviceSession)), c.t.Add(time.Minute), "e"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("new session for a removed device: %v", err)
	}
	if _, ok, _ := st.LiveTombstone(ctx, dev); !ok {
		t.Fatal("no tombstone")
	}
	// Removal is idempotent.
	if err := st.RemoveDevice(ctx, org, dev, owner, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmRemoval(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmRemoval(ctx, dev); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second confirm: %v", err)
	}

	// Tombstone expiry: the device becomes unknown at expires_at.
	dev2 := enroll(t, st, org)
	if err := st.RemoveDevice(ctx, org, dev2, owner, ""); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(store.TombstoneLifetime + time.Second)
	if _, ok, _ := st.LiveTombstone(ctx, dev2); ok {
		t.Fatal("expired tombstone still live")
	}
	res, err := st.RunRetention(ctx)
	if err != nil || res.Tombstones != 1 {
		t.Fatalf("retention: %+v %v", res, err)
	}
}

func TestOrganizationDeletionKeepsTombstonesAndAudit(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	org := newOrg(t, st)
	dev := enroll(t, st, org)
	key := secret.New(secret.KindAPIKey)
	if _, err := st.CreateAPIKey(ctx, org, "ci", owner, secret.Hash(key), secret.DisplayPrefix(key), ""); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrganization(ctx, org, owner, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LiveTombstone(ctx, dev); !ok {
		t.Fatal("tombstone lost with the organization")
	}
	if _, err := st.GetMembership(ctx, org, owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("membership survived")
	}
	if _, err := st.LookupAPIKey(ctx, secret.Hash(key)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal("API key survived")
	}
	var audits int
	_ = st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE organization_id = $1`, org).Scan(&audits)
	if audits < 4 {
		t.Fatalf("audit events: %d", audits)
	}
	jobs, err := st.DueCleanupJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Prefix != store.OrganizationPrefix(org) {
		t.Fatalf("clean-up job: %+v %v", jobs, err)
	}
	// The final sweep after organization deletion deletes the job state.
	done, err := st.RecordCleanupAttempt(ctx, jobs[0].ID, time.Now().Add(time.Hour), nil)
	if err != nil || !done {
		t.Fatalf("final sweep: %v %v", done, err)
	}
	if err := st.DeleteOrganization(ctx, org, owner, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestCrossOrganizationIsolation(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	a, b := newOrg(t, st), newOrg(t, st)
	dev := enroll(t, st, a)
	key := secret.New(secret.KindAPIKey)
	k, _ := st.CreateAPIKey(ctx, a, "k", owner, secret.Hash(key), secret.DisplayPrefix(key), "")
	_, tok := newToken(t, st, a, time.Hour)
	inv, _ := st.CreateInvitation(ctx, a, ownsandv1.Role_ROLE_MEMBER, owner, secret.Hash(secret.New(secret.KindInvitation)), "osi_x", time.Hour, "")
	checks := map[string]error{
		"get device":        func() error { _, err := st.GetDevice(ctx, b, dev); return err }(),
		"update limits":     st.UpdateDeviceLimits(ctx, b, dev, store.Limits{VCPUCount: 1}, owner, ""),
		"remove device":     st.RemoveDevice(ctx, b, dev, owner, ""),
		"revoke key":        st.RevokeAPIKey(ctx, b, k.ID, owner, ""),
		"revoke token":      st.RevokeEnrollmentToken(ctx, b, tok.ID, owner, ""),
		"revoke invitation": st.RevokeInvitation(ctx, b, inv.ID, owner, ""),
		"malformed id":      func() error { _, err := st.GetDevice(ctx, a, "not-a-uuid"); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	list, _ := st.ListDevices(ctx, b, true, store.Page{Size: 50})
	if len(list) != 0 {
		t.Fatal("devices of another organization listed")
	}
}

func TestInvitationsAndMembership(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	org := newOrg(t, st)
	v := secret.New(secret.KindInvitation)
	if _, err := st.CreateInvitation(ctx, org, ownsandv1.Role_ROLE_ADMIN, owner, secret.Hash(v), secret.DisplayPrefix(v), 7*24*time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	// An existing member gets already_exists and the invitation stays unused.
	if _, _, err := st.AcceptInvitation(ctx, secret.Hash(v), owner.ID, ""); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("owner accepts: %v", err)
	}
	if _, m, err := st.AcceptInvitation(ctx, secret.Hash(v), "user-2", ""); err != nil || m.Role != ownsandv1.Role_ROLE_ADMIN {
		t.Fatalf("accept: %v", err)
	}
	if _, _, err := st.AcceptInvitation(ctx, secret.Hash(v), "user-3", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("used invitation: %v", err)
	}
	exp := secret.New(secret.KindInvitation)
	_, _ = st.CreateInvitation(ctx, org, ownsandv1.Role_ROLE_MEMBER, owner, secret.Hash(exp), "osi_", time.Minute, "")
	c.t = c.t.Add(time.Hour)
	if _, _, err := st.AcceptInvitation(ctx, secret.Hash(exp), "user-3", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired invitation: %v", err)
	}
	// The owner cannot be demoted or removed; transfer makes the old owner admin.
	if _, err := st.UpdateMemberRole(ctx, org, owner.ID, ownsandv1.Role_ROLE_MEMBER, owner, ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("demote owner: %v", err)
	}
	if err := st.RemoveMember(ctx, org, owner.ID, owner, ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("remove owner: %v", err)
	}
	if err := st.TransferOwnership(ctx, org, owner, "user-2", ""); err != nil {
		t.Fatal(err)
	}
	m, _ := st.GetMembership(ctx, org, owner.ID)
	m2, _ := st.GetMembership(ctx, org, "user-2")
	if m.Role != ownsandv1.Role_ROLE_ADMIN || m2.Role != ownsandv1.Role_ROLE_OWNER {
		t.Fatalf("roles after transfer: %v %v", m.Role, m2.Role)
	}
	events, _ := st.ControlEventsAfter(ctx, 0, 100)
	before := len(events)
	if err := st.RemoveMember(ctx, org, owner.ID, owner, ""); err != nil {
		t.Fatal(err)
	}
	events, _ = st.ControlEventsAfter(ctx, 0, 100)
	if len(events) != before+1 || events[len(events)-1].Kind != store.ControlEventMembershipEnded || events[len(events)-1].UserID != owner.ID {
		t.Fatalf("membership revocation event: %+v", events)
	}
}

func TestLease(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	ok, err := st.AcquireLease(ctx, "l", "a", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("a: %v %v", ok, err)
	}
	if ok, _ := st.AcquireLease(ctx, "l", "b", 30*time.Second); ok {
		t.Fatal("b took a held lease")
	}
	if ok, _ := st.AcquireLease(ctx, "l", "a", 30*time.Second); !ok {
		t.Fatal("a could not renew")
	}
	_ = st.ReleaseLease(ctx, "l", "a")
	if ok, _ := st.AcquireLease(ctx, "l", "b", 30*time.Second); !ok {
		t.Fatal("b could not take a released lease")
	}
	_, _ = st.Pool.Exec(ctx, `UPDATE service_leases SET expires_at = now() - interval '1 second'`)
	if ok, _ := st.AcquireLease(ctx, "l", "a", 30*time.Second); !ok {
		t.Fatal("a could not take an expired lease")
	}
}

// Secret values never reach the database in plain text, and audit events
// carry no secret.
func TestNoPlainSecretsStored(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	org := newOrg(t, st)
	secrets := []string{secret.New(secret.KindAPIKey), secret.New(secret.KindInvitation), secret.New(secret.KindEnrollmentToken), secret.New(secret.KindDeviceSession)}
	_, _ = st.CreateAPIKey(ctx, org, "k", owner, secret.Hash(secrets[0]), secret.DisplayPrefix(secrets[0]), "")
	_, _ = st.CreateInvitation(ctx, org, ownsandv1.Role_ROLE_MEMBER, owner, secret.Hash(secrets[1]), secret.DisplayPrefix(secrets[1]), time.Hour, "")
	_, _ = st.CreateEnrollmentToken(ctx, org, "d", time.Hour, owner, secret.Hash(secrets[2]), secret.DisplayPrefix(secrets[2]), "")
	dev, _, err := st.Enroll(ctx, secret.Hash(secrets[2]), newKey(t), &ownsandv1.DeviceCapabilities{}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.CreateDeviceSession(ctx, dev, secret.Hash(secrets[3]), time.Now().Add(time.Minute), "e")
	var dump string
	err = st.Pool.QueryRow(ctx, `SELECT concat_ws('|',
		(SELECT string_agg(t::text, '|') FROM api_keys t), (SELECT string_agg(t::text, '|') FROM invitations t),
		(SELECT string_agg(t::text, '|') FROM enrollment_tokens t), (SELECT string_agg(t::text, '|') FROM device_sessions t),
		(SELECT string_agg(t::text, '|') FROM audit_events t))`).Scan(&dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(dump, s) || strings.Contains(dump, s[secret.DisplayPrefixLen:]) {
			t.Fatalf("plain secret stored")
		}
	}
}
