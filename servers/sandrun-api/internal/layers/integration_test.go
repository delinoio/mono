// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/secrets"
)

func TestCreateAndUpdateAgentIntegrationValidateCursorPoolName(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	admin := w.env.User(w.org.Admin).Integration
	const invalidPool = `team"a`
	assertInvalidPool := func(t *testing.T, err error) {
		t.Helper()
		const message = "invalid settings.cursor.pool_name: must not contain a double quote"
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeInvalidArgument || connectErr.Message() != message {
			t.Fatalf("got %v, want invalid_argument with %q", err, message)
		}
	}
	create := func(pool string) (*connect.Response[sandrunv1.CreateAgentIntegrationResponse], error) {
		return admin.CreateAgentIntegration(t.Context(), connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{
			OrganizationId: w.org.ID,
			Vendor:         sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR,
			Name:           "cursor",
			Settings:       &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: pool}},
			Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{
				CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "test-key"},
			},
		}))
	}

	_, err := create(invalidPool)
	assertInvalidPool(t, err)
	const originalPool = "Bob’s pool"
	created, err := create(originalPool)
	if err != nil || created.Msg.GetIntegration().GetCursor().GetPoolName() != originalPool {
		t.Fatalf("create with a supported pool name = %v, %v", created, err)
	}
	update := func(pool string) (*connect.Response[sandrunv1.UpdateAgentIntegrationResponse], error) {
		return admin.UpdateAgentIntegration(t.Context(), connect.NewRequest(&sandrunv1.UpdateAgentIntegrationRequest{
			OrganizationId:     w.org.ID,
			AgentIntegrationId: created.Msg.GetIntegration().GetAgentIntegrationId(),
			Settings:           &sandrunv1.UpdateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: pool}},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"settings"}},
		}))
	}
	_, err = update(invalidPool)
	assertInvalidPool(t, err)
	const updatedPool = "Bob's pool"
	updated, err := update(updatedPool)
	if err != nil || updated.Msg.GetIntegration().GetCursor().GetPoolName() != updatedPool {
		t.Fatalf("update with a supported pool name = %v, %v", updated, err)
	}
}

func TestAgentIntegrations(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	create := func(req *sandrunv1.CreateAgentIntegrationRequest) (*sandrunv1.AgentIntegration, error) {
		req.OrganizationId = w.org.ID
		resp, err := admin.Integration.CreateAgentIntegration(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetIntegration(), nil
	}
	secretsUsed := []string{"env-key-claude", "cursor-key", "codex-env", "codex-hook", "devin-token", "outpost-token", "codex-api-key"}
	claude, err := create(&sandrunv1.CreateAgentIntegrationRequest{Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &sandrunv1.ClaudeCodeCredentials{EnvironmentKey: secretsUsed[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := create(&sandrunv1.CreateAgentIntegrationRequest{Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: secretsUsed[1]}}})
	if err != nil {
		t.Fatal(err)
	}
	codex, err := create(&sandrunv1.CreateAgentIntegrationRequest{Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, Name: "codex",
		Settings: &sandrunv1.CreateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: &sandrunv1.OpenAiCodexSettings{}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: &sandrunv1.OpenAiCodexCredentials{
			EnvironmentKey: secretsUsed[2], WebhookSecret: secretsUsed[3], ApiKey: secretsUsed[6]}}})
	if err != nil {
		t.Fatal(err)
	}
	devin, err := create(&sandrunv1.CreateAgentIntegrationRequest{Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
		Settings: &sandrunv1.CreateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "outpost-a"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &sandrunv1.DevinCredentials{
			ApiToken: secretsUsed[4], OutpostToken: secretsUsed[5]}}})
	if err != nil {
		t.Fatal(err)
	}
	if codex.GetWebhookUrl() != "https://sandrun.test/webhooks/openai/"+codex.GetAgentIntegrationId() || claude.GetWebhookUrl() != "" {
		t.Fatalf("webhook URL %q", codex.GetWebhookUrl())
	}
	if c := codex.GetCredentials(); len(c) != 3 || c[2].GetCredential() != sandrunv1.AgentCredential_AGENT_CREDENTIAL_API_KEY || !c[2].GetCredentialSet() {
		t.Fatalf("Codex credentials %v", c)
	}
	if len(devin.GetCredentials()) != 2 || !devin.GetCredentials()[0].GetCredentialSet() || devin.GetCredentials()[0].GetCredentialSetAt() == nil ||
		cursor.GetCursor().GetPoolName() != "pool-a" || devin.GetDevin().GetOutpostName() != "outpost-a" {
		t.Fatalf("integration %v / %v", devin, cursor)
	}
	// Reads never return credentials, and the database holds them encrypted.
	member := w.env.User(w.org.Member).Integration
	expected := []*sandrunv1.AgentIntegration{claude, cursor, codex, devin}
	slices.SortFunc(expected, func(a, b *sandrunv1.AgentIntegration) int {
		return strings.Compare(a.GetAgentIntegrationId(), b.GetAgentIntegrationId())
	})
	list, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&sandrunv1.ListAgentIntegrationsRequest{OrganizationId: w.org.ID}))
	if err != nil || !slices.EqualFunc(list.Msg.GetIntegrations(), expected, equalIntegration) {
		t.Fatalf("list %v %v", list, err)
	}
	pageToken := ""
	for _, want := range expected {
		id := want.GetAgentIntegrationId()
		page, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&sandrunv1.ListAgentIntegrationsRequest{
			OrganizationId: w.org.ID, PageSize: 1, PageToken: pageToken,
		}))
		if err != nil || !slices.EqualFunc(page.Msg.GetIntegrations(), []*sandrunv1.AgentIntegration{want}, equalIntegration) ||
			page.Msg.GetNextPageToken() != api.NextPageToken(1, 1, id) {
			t.Fatalf("page for %s: %v, %v", id, page, err)
		}
		pageToken = page.Msg.GetNextPageToken()
		get, err := member.GetAgentIntegration(ctx, connect.NewRequest(&sandrunv1.GetAgentIntegrationRequest{
			OrganizationId: w.org.ID, AgentIntegrationId: id,
		}))
		if err != nil || !proto.Equal(get.Msg.GetIntegration(), want) {
			t.Fatalf("get %s: %v, %v", id, get, err)
		}
	}
	last, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&sandrunv1.ListAgentIntegrationsRequest{
		OrganizationId: w.org.ID, PageSize: 1, PageToken: pageToken,
	}))
	if err != nil || len(last.Msg.GetIntegrations()) != 0 || last.Msg.GetNextPageToken() != "" {
		t.Fatalf("last page: %v, %v", last, err)
	}
	outsider := w.env.User("outsider").Integration
	_, err = outsider.GetAgentIntegration(ctx, connect.NewRequest(&sandrunv1.GetAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(),
	}))
	wantCode(t, "outsider gets", err, connect.CodeNotFound)
	_, err = outsider.ListAgentIntegrations(ctx, connect.NewRequest(&sandrunv1.ListAgentIntegrationsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "outsider lists", err, connect.CodeNotFound)
	rows, err := w.env.DB.Pool.Query(ctx, "SELECT ciphertext FROM agent_integration_credentials")
	if err != nil {
		t.Fatalf("query stored credentials: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var ciphertext []byte
		if err := rows.Scan(&ciphertext); err != nil {
			t.Fatalf("scan stored credential: %v", err)
		}
		n++
		for _, s := range secretsUsed {
			if bytes.Contains(ciphertext, []byte(s)) {
				t.Fatal("credential stored in clear text")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored credentials: %v", err)
	}
	if n != 7 {
		t.Fatalf("%d stored credentials", n)
	}
	for _, s := range secretsUsed {
		if strings.Contains(list.Msg.String(), s) {
			t.Fatalf("credential %q in a response", s)
		}
	}
	if v, err := w.l.Integrations.Credential(ctx, w.org.ID, devin.GetAgentIntegrationId(), sandrunv1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN); err != nil || v != "outpost-token" {
		t.Fatalf("decrypt %q %v", v, err)
	}
	if _, err := w.l.Integrations.Credential(ctx, w.env.NewOrg("bob").ID, devin.GetAgentIntegrationId(), sandrunv1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("credential of another organization: %v", err)
	}

	// Validation.
	for name, req := range map[string]*sandrunv1.CreateAgentIntegrationRequest{
		"no vendor": {Name: "x", Settings: &sandrunv1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}}},
		"wrong settings": {Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "x",
			Settings:    &sandrunv1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}},
			Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "k"}}},
		"missing credential": {Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "x",
			Settings:    &sandrunv1.CreateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "o"}},
			Credentials: &sandrunv1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &sandrunv1.DevinCredentials{ApiToken: "t"}}},
		"empty pool": {Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "x",
			Settings:    &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{}},
			Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "k"}}},
		"long name": {Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: strings.Repeat("n", 65),
			Settings:    &sandrunv1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}},
			Credentials: &sandrunv1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &sandrunv1.ClaudeCodeCredentials{EnvironmentKey: "k"}}},
	} {
		_, err := create(req)
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	_, err = create(&sandrunv1.CreateAgentIntegrationRequest{Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &sandrunv1.ClaudeCodeCredentials{EnvironmentKey: "k"}}})
	wantCode(t, "duplicate name", err, connect.CodeAlreadyExists)
	_, err = w.env.User(w.org.Member).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID}))
	wantCode(t, "member creates", err, connect.CodePermissionDenied)

	// Update the name, Devin settings and one credential with descriptor-valid paths.
	before := devin.GetCredentials()[1].GetCredentialSetAt().AsTime()
	w.env.Clock.Advance(time.Second)
	devinUpdate := &sandrunv1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(), Name: "devin-2",
		Settings:    &sandrunv1.UpdateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "outpost-b"}},
		Credentials: &sandrunv1.UpdateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &sandrunv1.DevinCredentials{OutpostToken: "outpost-token-2"}},
	}
	devinUpdate.UpdateMask, err = fieldmaskpb.New(devinUpdate, "name", "devin", "devin_credentials")
	if err != nil {
		t.Fatal(err)
	}
	up, err := admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(devinUpdate))
	if err != nil {
		t.Fatal(err)
	}
	in := up.Msg.GetIntegration()
	if in.GetName() != "devin-2" || in.GetDevin().GetOutpostName() != "outpost-b" || !in.GetCredentials()[1].GetCredentialSetAt().AsTime().After(before) ||
		!in.GetCredentials()[0].GetCredentialSetAt().AsTime().Equal(devin.GetCredentials()[0].GetCredentialSetAt().AsTime()) {
		t.Fatalf("updated %v", in)
	}
	if v, _ := w.l.Integrations.Credential(ctx, w.org.ID, devin.GetAgentIntegrationId(), sandrunv1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN); v != "devin-token" {
		t.Fatal("an empty credential field replaced the stored value")
	}
	_, err = admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(),
		Credentials: &sandrunv1.UpdateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "k"}},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"credentials"}}}))
	wantCode(t, "credentials of another vendor", err, connect.CodeInvalidArgument)
	_, err = admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"vendor"}}}))
	wantCode(t, "unknown path", err, connect.CodeInvalidArgument)
	if countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_UPDATED) != 1 ||
		countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREDENTIALS_REPLACED) != 1 ||
		countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREATED) != 4 {
		t.Fatal("audit events")
	}

	getIntegration := func(t *testing.T, want *sandrunv1.AgentIntegration) {
		t.Helper()
		got, err := admin.Integration.GetAgentIntegration(ctx, connect.NewRequest(&sandrunv1.GetAgentIntegrationRequest{
			OrganizationId: w.org.ID, AgentIntegrationId: want.GetAgentIntegrationId(),
		}))
		if err != nil || !proto.Equal(got.Msg.GetIntegration(), want) {
			t.Fatalf("persisted integration differs from expected state: %v", err)
		}
	}
	getIntegration(t, in)
	type auditCounts struct{ updates, replacements int }
	audits := func(t *testing.T) auditCounts {
		t.Helper()
		return auditCounts{
			updates:      countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_UPDATED),
			replacements: countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREDENTIALS_REPLACED),
		}
	}
	updateIntegration := func(t *testing.T, req *sandrunv1.UpdateAgentIntegrationRequest, wantDelta auditCounts) *sandrunv1.AgentIntegration {
		t.Helper()
		start := audits(t)
		w.env.Clock.Advance(time.Second)
		resp, err := admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		want := auditCounts{updates: start.updates + wantDelta.updates, replacements: start.replacements + wantDelta.replacements}
		if got := audits(t); got != want {
			t.Fatalf("audit counts = %+v, want %+v", got, want)
		}
		return resp.Msg.GetIntegration()
	}
	checkRotation := func(t *testing.T, previous, updated *sandrunv1.AgentIntegration, kind sandrunv1.AgentCredential, value string) {
		t.Helper()
		if len(updated.GetCredentials()) != len(previous.GetCredentials()) {
			t.Fatal("unexpected number of credential states")
		}
		found := false
		for i, state := range updated.GetCredentials() {
			if state.GetCredential() != kind {
				if !proto.Equal(state, previous.GetCredentials()[i]) {
					t.Fatal("an omitted credential changed state")
				}
				continue
			}
			if !state.GetCredentialSet() || !proto.Equal(state.GetCredentialSetAt(), updated.GetUpdateTime()) ||
				!state.GetCredentialSetAt().AsTime().After(previous.GetCredentials()[i].GetCredentialSetAt().AsTime()) {
				t.Fatal("replacement credential timestamp did not advance to the update time")
			}
			found = true
		}
		if !found {
			t.Fatal("missing replacement credential state")
		}
		got, err := w.l.Integrations.Credential(ctx, w.org.ID, updated.GetAgentIntegrationId(), kind)
		if err != nil || got != value || strings.Contains(updated.String(), value) {
			t.Fatalf("unexpected stored credential or response state: %v", err)
		}
	}
	checkRotation(t, devin, in, sandrunv1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN, "outpost-token-2")

	const newEnvironmentKey = "test-environment-replacement"
	for _, tc := range []struct {
		old                           *sandrunv1.AgentIntegration
		settingsPath, credentialsPath string
		req                           *sandrunv1.UpdateAgentIntegrationRequest
	}{
		{claude, "claude_code", "claude_code_credentials", &sandrunv1.UpdateAgentIntegrationRequest{
			Settings: &sandrunv1.UpdateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &sandrunv1.ClaudeCodeSettings{}},
			Credentials: &sandrunv1.UpdateAgentIntegrationRequest_ClaudeCodeCredentials{
				ClaudeCodeCredentials: &sandrunv1.ClaudeCodeCredentials{EnvironmentKey: newEnvironmentKey}},
		}},
		{codex, "openai_codex", "openai_codex_credentials", &sandrunv1.UpdateAgentIntegrationRequest{
			Settings: &sandrunv1.UpdateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: &sandrunv1.OpenAiCodexSettings{}},
			Credentials: &sandrunv1.UpdateAgentIntegrationRequest_OpenaiCodexCredentials{
				OpenaiCodexCredentials: &sandrunv1.OpenAiCodexCredentials{EnvironmentKey: newEnvironmentKey}},
		}},
	} {
		t.Run(tc.settingsPath, func(t *testing.T) {
			req := tc.req
			req.OrganizationId, req.AgentIntegrationId = w.org.ID, tc.old.GetAgentIntegrationId()
			mask, err := fieldmaskpb.New(req, tc.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			req.UpdateMask = mask
			settingsOnly := updateIntegration(t, req, auditCounts{updates: 1})
			want := proto.CloneOf(tc.old)
			want.UpdateTime = settingsOnly.GetUpdateTime()
			if !proto.Equal(settingsOnly, want) {
				t.Fatal("settings-only update changed other integration state")
			}

			req.UpdateMask, err = fieldmaskpb.New(req, tc.credentialsPath)
			if err != nil {
				t.Fatal(err)
			}
			rotated := updateIntegration(t, req, auditCounts{replacements: 1})
			checkRotation(t, settingsOnly, rotated, sandrunv1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, newEnvironmentKey)
			getIntegration(t, rotated)
		})
	}

	// Use fresh Cursor fixtures for each path combination.
	const newCursorKey = "test-cursor-replacement"
	cursorCredential := sandrunv1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY
	cursorSettings := &sandrunv1.UpdateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool-b"}}
	cursorCredentials := &sandrunv1.UpdateAgentIntegrationRequest_CursorCredentials{
		CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: newCursorKey}}
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"canonical", []string{"cursor", "cursor_credentials"}},
		{"aliases", []string{"settings", "credentials"}},
		{"canonical first", []string{"cursor", "cursor_credentials", "settings", "credentials"}},
		{"aliases first", []string{"settings", "credentials", "cursor", "cursor_credentials"}},
		{"repeated paths", []string{"cursor", "cursor", "cursor_credentials", "cursor_credentials"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh, err := create(&sandrunv1.CreateAgentIntegrationRequest{
				Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: tc.name,
				Settings: &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool-a"}},
				Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{
					CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "initial-test-key"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := &sandrunv1.UpdateAgentIntegrationRequest{
				OrganizationId: w.org.ID, AgentIntegrationId: fresh.GetAgentIntegrationId(),
				Settings: cursorSettings, Credentials: cursorCredentials,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: tc.paths},
			}
			if tc.name == "canonical" {
				req.UpdateMask, err = fieldmaskpb.New(req, tc.paths...)
				if err != nil {
					t.Fatal(err)
				}
			}
			updated := updateIntegration(t, req, auditCounts{updates: 1, replacements: 1})
			if updated.GetCursor().GetPoolName() != "pool-b" {
				t.Fatal("Cursor pool was not updated")
			}
			checkRotation(t, fresh, updated, cursorCredential, newCursorKey)
			getIntegration(t, updated)
		})
	}

	devinSettings := &sandrunv1.UpdateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "outpost"}}
	devinCredentials := &sandrunv1.UpdateAgentIntegrationRequest_DevinCredentials{
		DevinCredentials: &sandrunv1.DevinCredentials{ApiToken: "test-token"}}
	for _, tc := range []struct {
		name  string
		paths []string
		req   *sandrunv1.UpdateAgentIntegrationRequest
	}{
		{"missing settings", []string{"cursor"}, &sandrunv1.UpdateAgentIntegrationRequest{}},
		{"wrong settings member", []string{"cursor"}, &sandrunv1.UpdateAgentIntegrationRequest{Settings: devinSettings}},
		{"wrong settings vendor", []string{"devin"}, &sandrunv1.UpdateAgentIntegrationRequest{Settings: devinSettings}},
		{"alias with wrong settings path", []string{"name", "settings", "devin"}, &sandrunv1.UpdateAgentIntegrationRequest{Settings: cursorSettings}},
		{"alias with wrong settings member", []string{"settings", "cursor"}, &sandrunv1.UpdateAgentIntegrationRequest{Settings: devinSettings}},
		{"missing credentials", []string{"cursor_credentials"}, &sandrunv1.UpdateAgentIntegrationRequest{}},
		{"wrong credential member", []string{"cursor_credentials"}, &sandrunv1.UpdateAgentIntegrationRequest{Credentials: devinCredentials}},
		{"wrong credential vendor", []string{"devin_credentials"}, &sandrunv1.UpdateAgentIntegrationRequest{Credentials: devinCredentials}},
		{"alias with wrong credential path", []string{"devin_credentials", "credentials"}, &sandrunv1.UpdateAgentIntegrationRequest{Credentials: cursorCredentials}},
		{"empty credential values", []string{"cursor_credentials"}, &sandrunv1.UpdateAgentIntegrationRequest{Credentials: &sandrunv1.UpdateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{}}}},
		{"empty mask", nil, &sandrunv1.UpdateAgentIntegrationRequest{Settings: cursorSettings}},
		{"nested path", []string{"cursor.pool_name"}, &sandrunv1.UpdateAgentIntegrationRequest{Settings: cursorSettings}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := audits(t)
			req := tc.req
			req.OrganizationId, req.AgentIntegrationId = w.org.ID, cursor.GetAgentIntegrationId()
			req.Name = "rejected rename"
			req.UpdateMask = &fieldmaskpb.FieldMask{Paths: tc.paths}
			_, err := admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(req))
			wantCode(t, tc.name, err, connect.CodeInvalidArgument)
			getIntegration(t, cursor)
			if got := audits(t); got != start {
				t.Fatalf("rejected update changed audit counts from %+v to %+v", start, got)
			}
		})
	}

	if v, err := w.l.Integrations.Credential(ctx, w.org.ID, cursor.GetAgentIntegrationId(), cursorCredential); err != nil || v != secretsUsed[1] {
		t.Fatalf("rejected update changed the stored Cursor credential: %v", err)
	}

	// Key rotation: the re-encryption job reseals all seven original credentials and five Cursor fixtures.
	newKey, _ := secrets.GenerateKey("next")
	ring, _ := secrets.NewKeyring(append([]secrets.Key{newKey}, keyOf(t, w)...)...)
	changed, err := w.l.Integrations.Reencrypt(ctx, ring)
	if err != nil || changed != 12 {
		t.Fatalf("resealed %d, %v", changed, err)
	}
	var left int
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_integration_credentials WHERE key_id <> 'next'").Scan(&left); err != nil {
		t.Fatalf("count credentials with the old key: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d credentials keep the old key", left)
	}
	if again, err := w.l.Integrations.Reencrypt(ctx, ring); err != nil || again != 0 {
		t.Fatalf("second run %d %v", again, err)
	}

}

func equalIntegration(a, b *sandrunv1.AgentIntegration) bool {
	return proto.Equal(a, b)
}

func TestAgentIntegrationReadSnapshot(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"update", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			w := newWorld(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			admin := w.env.User(w.org.Admin).Integration
			created, err := admin.CreateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{
				OrganizationId: w.org.ID, Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
				Settings: &sandrunv1.CreateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "original"}},
				Credentials: &sandrunv1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &sandrunv1.DevinCredentials{
					ApiToken: "test-api-token", OutpostToken: "test-outpost-token",
				}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			before := created.Msg.GetIntegration()
			id := before.GetAgentIntegrationId()

			tx, err := w.env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// Pause each RPC after it reads the integration row and before it reads credential state.
			if _, err := tx.Exec(ctx, "LOCK TABLE agent_integration_credentials IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			updateTime := before.GetUpdateTime().AsTime().Add(time.Second)
			if mutation == "update" {
				if _, err := tx.Exec(ctx, "UPDATE agent_integrations SET outpost_name = 'updated', update_time = $2 WHERE agent_integration_id = $1", id, updateTime); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "UPDATE agent_integration_credentials SET set_time = $2 WHERE agent_integration_id = $1", id, updateTime); err != nil {
					t.Fatal(err)
				}
			} else if _, err := tx.Exec(ctx, "DELETE FROM agent_integrations WHERE agent_integration_id = $1", id); err != nil {
				t.Fatal(err)
			}

			member := w.env.User(w.org.Member).Integration
			get := func() (*sandrunv1.AgentIntegration, error) {
				res, err := member.GetAgentIntegration(ctx, connect.NewRequest(&sandrunv1.GetAgentIntegrationRequest{
					OrganizationId: w.org.ID, AgentIntegrationId: id,
				}))
				if err != nil {
					return nil, err
				}
				return res.Msg.GetIntegration(), nil
			}
			list := func() ([]*sandrunv1.AgentIntegration, error) {
				res, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&sandrunv1.ListAgentIntegrationsRequest{
					OrganizationId: w.org.ID, PageSize: 1,
				}))
				if err != nil {
					return nil, err
				}
				return res.Msg.GetIntegrations(), nil
			}
			var (
				gotIntegration  *sandrunv1.AgentIntegration
				gotPage         []*sandrunv1.AgentIntegration
				getErr, listErr error
				reads           sync.WaitGroup
			)
			reads.Go(func() { gotIntegration, getErr = get() })
			reads.Go(func() { gotPage, listErr = list() })

			for {
				var waiting int
				err := w.env.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks AS l JOIN pg_stat_activity AS a USING (pid)
					WHERE a.datname = current_database() AND a.query LIKE 'SELECT credential, set_time FROM agent_integration_credentials%'
					  AND l.relation = 'agent_integration_credentials'::regclass AND l.mode = 'AccessShareLock' AND NOT l.granted`).Scan(&waiting)
				if err != nil {
					t.Fatalf("wait for both credential reads: %v", err)
				}
				if waiting == 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			reads.Wait()
			if getErr != nil || !proto.Equal(gotIntegration, before) {
				t.Errorf("Get in flight: got %v, %v; want %v", gotIntegration, getErr, before)
			}
			if listErr != nil || !slices.EqualFunc(gotPage, []*sandrunv1.AgentIntegration{before}, equalIntegration) {
				t.Errorf("List in flight: got %v, %v; want [%v]", gotPage, listErr, before)
			}

			after, err := get()
			afterList, listErr := list()
			if mutation == "delete" {
				wantCode(t, "Get after delete", err, connect.CodeNotFound)
				if listErr != nil || len(afterList) != 0 {
					t.Errorf("List after delete: got %v, %v; want an empty page", afterList, listErr)
				}
				return
			}
			expected := proto.CloneOf(before)
			expected.GetDevin().OutpostName = "updated"
			expected.UpdateTime = timestamppb.New(updateTime)
			for _, state := range expected.GetCredentials() {
				state.CredentialSetAt = expected.UpdateTime
			}
			if err != nil || !proto.Equal(after, expected) {
				t.Errorf("Get after update: got %v, %v; want %v", after, err, expected)
			}
			if listErr != nil || !slices.EqualFunc(afterList, []*sandrunv1.AgentIntegration{expected}, equalIntegration) {
				t.Errorf("List after update: got %v, %v; want [%v]", afterList, listErr, expected)
			}
		})
	}
}

// keyOf returns the test environment's key, so the rotation ring can still
// open the values that it sealed.
func keyOf(t *testing.T, w *world) []secrets.Key {
	t.Helper()
	return []secrets.Key{w.env.SecretKey}
}

// A credential or OwnSand key that is replaced after the re-encryption job
// read it keeps its new value: the replacement on an instance that still
// has the old primary key keeps the key ID, so the job must not write the
// value that it read before.
func TestReencryptKeepsConcurrentReplacement(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	resp, err := w.env.User(w.org.Admin).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{
		OrganizationId: w.org.ID, Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "cursor-key"}}}))
	if err != nil {
		t.Fatal(err)
	}
	integrationID := resp.Msg.GetIntegration().GetAgentIntegrationId()
	osOrg := api.NewID()
	w.fake.AddOrg(osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	newKey, _ := secrets.GenerateKey("next")
	ring, _ := secrets.NewKeyring(append([]secrets.Key{newKey}, keyOf(t, w)...)...)
	replaced := []byte("replaced")
	for _, tc := range []struct {
		name, hold, read, reseal string
		arg                      string
		run                      func() (int64, error)
	}{
		{"credential", "UPDATE agent_integration_credentials SET ciphertext = $2 WHERE agent_integration_id = $1",
			"SELECT key_id, ciphertext FROM agent_integration_credentials WHERE agent_integration_id = $1",
			"UPDATE agent_integration_credentials SET key_id = $3, ciphertext = $4", integrationID,
			func() (int64, error) { return w.l.Integrations.Reencrypt(ctx, ring) }},
		{"ownsand link", "UPDATE ownsand_links SET api_key_ciphertext = $2 WHERE organization_id = $1",
			"SELECT api_key_key_id, api_key_ciphertext FROM ownsand_links WHERE organization_id = $1",
			"UPDATE ownsand_links SET api_key_key_id = $2, api_key_ciphertext = $3", w.org.ID,
			func() (int64, error) { return w.l.Links.Reencrypt(ctx, ring) }},
	} {
		// The held update stands in for a replacement that commits after
		// the job's scan.
		holder := holdTx(t, w.env, tc.hold, tc.arg, replaced)
		type result struct {
			n   int64
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := tc.run()
			done <- result{n, err}
		}()
		waitForLockWaiter(t, w.env, lockWait{blockerPID: holder.pid, query: tc.reseal})
		holder.commit()
		if r := <-done; r.err != nil || r.n != 0 {
			t.Fatalf("%s: resealed %d, %v; want 0", tc.name, r.n, r.err)
		}
		var keyID string
		var ciphertext []byte
		if err := w.env.DB.Pool.QueryRow(ctx, tc.read, tc.arg).Scan(&keyID, &ciphertext); err != nil || keyID == "next" || !bytes.Equal(ciphertext, replaced) {
			t.Fatalf("%s: stored %q %q, %v; want the replacement", tc.name, keyID, ciphertext, err)
		}
	}
}
