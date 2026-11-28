// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"math"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
)

func TestInvitationToken(t *testing.T) {
	t.Parallel()
	token, hash, err := newInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "bai_") || len(token) != len("bai_")+43 || !wellFormedInvitationToken(token) {
		t.Fatalf("token %q", token)
	}
	if string(hash) != string(hashInvitationToken(token)) || len(hash) != 32 {
		t.Fatal("hash mismatch")
	}
	for _, bad := range []string{"", "bai_", "bai_" + strings.Repeat("A", 42), "bai_" + strings.Repeat("A", 44), "xyz_" + token[4:], "bai_" + strings.Repeat("+", 43)} {
		if wellFormedInvitationToken(bad) {
			t.Errorf("wellFormedInvitationToken(%q) = true", bad)
		}
	}
}

func TestInvitationState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := invitation{ExpireTime: now.Add(time.Hour)}
	if inv.state(now) != accountsv1.InvitationState_INVITATION_STATE_PENDING {
		t.Fatal("want pending")
	}
	if inv.state(now.Add(time.Hour)) != accountsv1.InvitationState_INVITATION_STATE_EXPIRED {
		t.Fatal("want expired at expire_time")
	}
	inv.RevokeTime = &now
	if inv.state(now) != accountsv1.InvitationState_INVITATION_STATE_REVOKED {
		t.Fatal("want revoked")
	}
	inv.RevokeTime, inv.AcceptTime = nil, &now
	if inv.state(now.Add(2*time.Hour)) != accountsv1.InvitationState_INVITATION_STATE_ACCEPTED {
		t.Fatal("want accepted")
	}
}

func TestPageSizeAndToken(t *testing.T) {
	t.Parallel()
	for in, want := range map[int32]int{0: 50, 1: 1, 200: 200, 201: 200} {
		if got, err := pageSize(in); err != nil || got != want {
			t.Errorf("pageSize(%d) = %d, %v", in, got, err)
		}
	}
	if _, err := pageSize(-1); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("pageSize(-1) error = %v", err)
	}
	raw := encodePageToken(pageToken{RPC: "R", Scope: "S", After: "a", AfterTime: 5})
	if tok, err := decodePageToken(raw, "R", "S"); err != nil || tok.After != "a" || tok.AfterTime != 5 {
		t.Fatalf("decode = %+v, %v", tok, err)
	}
	for _, forged := range []pageToken{
		{RPC: "R", Scope: "S", After: "a", AfterTime: -1},
		{RPC: "R", Scope: "S", After: "a", AfterTime: math.MinInt64},
		{RPC: "R", Scope: "S", After: "\x00"},
		{RPC: "R", Scope: "S", After: "a\x00b", AfterTime: 5},
		{RPC: "R", Scope: "S", After: "ownsand\n\x00\nmeter"},
	} {
		if _, err := decodePageToken(encodePageToken(forged), "R", "S"); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("decodePageToken(%+v) error = %v", forged, err)
		}
	}
	for _, valid := range []pageToken{
		{RPC: "R", Scope: "S", After: "a"},
		{RPC: "R", Scope: "S", After: "a\x01b", AfterTime: math.MaxInt64},
	} {
		if _, err := decodePageToken(encodePageToken(valid), "R", "S"); err != nil {
			t.Errorf("decodePageToken(%+v) error = %v", valid, err)
		}
	}
	for _, tc := range [][3]string{{raw, "R", "other"}, {raw, "Other", "S"}, {"%%%", "R", "S"}, {"e30", "R", "S"}} {
		if _, err := decodePageToken(tc[0], tc[1], tc[2]); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("decodePageToken(%q, %q, %q) error = %v", tc[0], tc[1], tc[2], err)
		}
	}
}

func TestFieldRules(t *testing.T) {
	t.Parallel()
	if name, err := billingAccountName("  Team \t"); err != nil || name != "Team" {
		t.Errorf("billingAccountName = %q, %v", name, err)
	}
	const ipv6Domain = "@[2001:db8::1]"
	maxLengthAddress := strings.Repeat("é", (maxEmailBytes-len(ipv6Domain))/2) + ipv6Domain
	for _, ok := range []string{
		"a@b",
		"first.last+tag@example.co.uk",
		" A@B.COM ",
		"a@[192.0.2.1]",
		"a@[2001:db8::1]",
		"a@[::ffff:c000:201]",
		" \tA@[2001:DB8::AB] ",
		maxLengthAddress,
	} {
		if got, err := invitationEmail(ok); err != nil || got != strings.TrimSpace(ok) {
			t.Errorf("invitationEmail(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"a", "a@", "@b", "a@b@c", "<a@b>", "x <a@b>", "a b@c",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", 190) + ".com",
		"a@[IPv6:2001:db8::1]",
		"a@[2001:db8::x]",
		"a@[fe80::1%eth0]",
		"a@[2001:db8::1",
		"a..b" + ipv6Domain,
		"a@b" + ipv6Domain,
		"x" + maxLengthAddress,
	} {
		if _, err := invitationEmail(bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("invitationEmail(%q) error = %v, want invalid_argument", bad, err)
		}
	}
	if emailKey("Ä.User@Example.COM") != "Ä.user@example.com" {
		t.Errorf("emailKey folds only ASCII: %q", emailKey("Ä.User@Example.COM"))
	}
	if id, err := normalizeID("x", "01900000-0000-7ABC-8DEF-000000000000"); err != nil || id != "01900000-0000-7abc-8def-000000000000" {
		t.Errorf("normalizeID = %q, %v", id, err)
	}
	if !RoleAdmin.Allows(PermissionManage) || RoleAdmin.Allows(PermissionOwn) || RoleViewer.Allows(PermissionManage) || !RoleViewer.Allows(PermissionRead) || Role("").Allows(PermissionRead) {
		t.Error("role permissions do not match the contract table")
	}
}

func TestUserIDField(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a", "abc\x01", "user-1", "사용자", strings.Repeat("a", maxUserIDBytes)} {
		if got, err := userIDField(ok); err != nil || got != ok {
			t.Errorf("userIDField(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "\x00", "abc\x00", "\x00abc", "ab\x00cd", "\xff", strings.Repeat("a", maxUserIDBytes+1)} {
		if _, err := userIDField(bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("userIDField(%q) error = %v", bad, err)
		}
	}
}
