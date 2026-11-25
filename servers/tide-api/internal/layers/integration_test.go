// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
)

func TestAgentIntegrations(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	create := func(req *tidev1.CreateAgentIntegrationRequest) (*tidev1.AgentIntegration, error) {
		req.OrganizationId = w.org.ID
		resp, err := admin.Integration.CreateAgentIntegration(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetIntegration(), nil
	}
	secretsUsed := []string{"env-key-claude", "cursor-key", "codex-env", "codex-hook", "devin-token", "outpost-token", "codex-api-key"}
	claude, err := create(&tidev1.CreateAgentIntegrationRequest{Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: secretsUsed[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := create(&tidev1.CreateAgentIntegrationRequest{Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: secretsUsed[1]}}})
	if err != nil {
		t.Fatal(err)
	}
	codex, err := create(&tidev1.CreateAgentIntegrationRequest{Vendor: tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, Name: "codex",
		Settings: &tidev1.CreateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: &tidev1.OpenAiCodexSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: &tidev1.OpenAiCodexCredentials{
			EnvironmentKey: secretsUsed[2], WebhookSecret: secretsUsed[3], ApiKey: secretsUsed[6]}}})
	if err != nil {
		t.Fatal(err)
	}
	devin, err := create(&tidev1.CreateAgentIntegrationRequest{Vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
		Settings: &tidev1.CreateAgentIntegrationRequest_Devin{Devin: &tidev1.DevinSettings{OutpostName: "outpost-a"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &tidev1.DevinCredentials{
			ApiToken: secretsUsed[4], OutpostToken: secretsUsed[5]}}})
	if err != nil {
		t.Fatal(err)
	}
	if codex.GetWebhookUrl() != "https://tide.test/webhooks/openai/"+codex.GetAgentIntegrationId() || claude.GetWebhookUrl() != "" {
		t.Fatalf("webhook URL %q", codex.GetWebhookUrl())
	}
	if c := codex.GetCredentials(); len(c) != 3 || c[2].GetCredential() != tidev1.AgentCredential_AGENT_CREDENTIAL_API_KEY || !c[2].GetCredentialSet() {
		t.Fatalf("Codex credentials %v", c)
	}
	if len(devin.GetCredentials()) != 2 || !devin.GetCredentials()[0].GetCredentialSet() || devin.GetCredentials()[0].GetCredentialSetAt() == nil ||
		cursor.GetCursor().GetPoolName() != "pool-a" || devin.GetDevin().GetOutpostName() != "outpost-a" {
		t.Fatalf("integration %v / %v", devin, cursor)
	}
	// Reads never return credentials, and the database holds them encrypted.
	member := w.env.User(w.org.Member).Integration
	expected := []*tidev1.AgentIntegration{claude, cursor, codex, devin}
	slices.SortFunc(expected, func(a, b *tidev1.AgentIntegration) int {
		return strings.Compare(a.GetAgentIntegrationId(), b.GetAgentIntegrationId())
	})
	list, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&tidev1.ListAgentIntegrationsRequest{OrganizationId: w.org.ID}))
	if err != nil || !slices.EqualFunc(list.Msg.GetIntegrations(), expected, equalIntegration) {
		t.Fatalf("list %v %v", list, err)
	}
	pageToken := ""
	for _, want := range expected {
		id := want.GetAgentIntegrationId()
		page, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&tidev1.ListAgentIntegrationsRequest{
			OrganizationId: w.org.ID, PageSize: 1, PageToken: pageToken,
		}))
		if err != nil || !slices.EqualFunc(page.Msg.GetIntegrations(), []*tidev1.AgentIntegration{want}, equalIntegration) ||
			page.Msg.GetNextPageToken() != api.NextPageToken(1, 1, id) {
			t.Fatalf("page for %s: %v, %v", id, page, err)
		}
		pageToken = page.Msg.GetNextPageToken()
		get, err := member.GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{
			OrganizationId: w.org.ID, AgentIntegrationId: id,
		}))
		if err != nil || !proto.Equal(get.Msg.GetIntegration(), want) {
			t.Fatalf("get %s: %v, %v", id, get, err)
		}
	}
	last, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&tidev1.ListAgentIntegrationsRequest{
		OrganizationId: w.org.ID, PageSize: 1, PageToken: pageToken,
	}))
	if err != nil || len(last.Msg.GetIntegrations()) != 0 || last.Msg.GetNextPageToken() != "" {
		t.Fatalf("last page: %v, %v", last, err)
	}
	outsider := w.env.User("outsider").Integration
	_, err = outsider.GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(),
	}))
	wantCode(t, "outsider gets", err, connect.CodeNotFound)
	_, err = outsider.ListAgentIntegrations(ctx, connect.NewRequest(&tidev1.ListAgentIntegrationsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "outsider lists", err, connect.CodeNotFound)
	rows, _ := w.env.DB.Pool.Query(ctx, "SELECT ciphertext FROM agent_integration_credentials")
	n := 0
	for rows.Next() {
		var c []byte
		_ = rows.Scan(&c)
		n++
		for _, s := range secretsUsed {
			if bytes.Contains(c, []byte(s)) {
				t.Fatalf("credential %q stored in clear text", s)
			}
		}
	}
	rows.Close()
	for _, s := range secretsUsed {
		if strings.Contains(list.Msg.String(), s) {
			t.Fatalf("credential %q in a response", s)
		}
	}
	if n != 7 {
		t.Fatalf("%d stored credentials", n)
	}
	if v, err := w.l.Integrations.Credential(ctx, w.org.ID, devin.GetAgentIntegrationId(), tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN); err != nil || v != "outpost-token" {
		t.Fatalf("decrypt %q %v", v, err)
	}
	if _, err := w.l.Integrations.Credential(ctx, w.env.NewOrg("bob").ID, devin.GetAgentIntegrationId(), tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("credential of another organization: %v", err)
	}

	// Validation.
	for name, req := range map[string]*tidev1.CreateAgentIntegrationRequest{
		"no vendor": {Name: "x", Settings: &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}}},
		"wrong settings": {Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "x",
			Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
			Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "k"}}},
		"missing credential": {Vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "x",
			Settings:    &tidev1.CreateAgentIntegrationRequest_Devin{Devin: &tidev1.DevinSettings{OutpostName: "o"}},
			Credentials: &tidev1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &tidev1.DevinCredentials{ApiToken: "t"}}},
		"empty pool": {Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "x",
			Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{}},
			Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "k"}}},
		"long name": {Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: strings.Repeat("n", 65),
			Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
			Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: "k"}}},
	} {
		_, err := create(req)
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	_, err = create(&tidev1.CreateAgentIntegrationRequest{Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: "k"}}})
	wantCode(t, "duplicate name", err, connect.CodeAlreadyExists)
	_, err = w.env.User(w.org.Member).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&tidev1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID}))
	wantCode(t, "member creates", err, connect.CodePermissionDenied)

	// Updates: name and settings, then one credential.
	before := devin.GetCredentials()[1].GetCredentialSetAt().AsTime()
	w.env.Clock.Advance(time.Second)
	up, err := admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(&tidev1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(), Name: "devin-2",
		Settings:    &tidev1.UpdateAgentIntegrationRequest_Devin{Devin: &tidev1.DevinSettings{OutpostName: "outpost-b"}},
		Credentials: &tidev1.UpdateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &tidev1.DevinCredentials{OutpostToken: "outpost-token-2"}},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"name", "settings", "credentials"}}}))
	if err != nil {
		t.Fatal(err)
	}
	in := up.Msg.GetIntegration()
	if in.GetName() != "devin-2" || in.GetDevin().GetOutpostName() != "outpost-b" || !in.GetCredentials()[1].GetCredentialSetAt().AsTime().After(before) ||
		!in.GetCredentials()[0].GetCredentialSetAt().AsTime().Equal(devin.GetCredentials()[0].GetCredentialSetAt().AsTime()) {
		t.Fatalf("updated %v", in)
	}
	if v, _ := w.l.Integrations.Credential(ctx, w.org.ID, devin.GetAgentIntegrationId(), tidev1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN); v != "devin-token" {
		t.Fatal("an empty credential field replaced the stored value")
	}
	_, err = admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(&tidev1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(),
		Credentials: &tidev1.UpdateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "k"}},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"credentials"}}}))
	wantCode(t, "credentials of another vendor", err, connect.CodeInvalidArgument)
	_, err = admin.Integration.UpdateAgentIntegration(ctx, connect.NewRequest(&tidev1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: devin.GetAgentIntegrationId(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"vendor"}}}))
	wantCode(t, "unknown path", err, connect.CodeInvalidArgument)
	if countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_UPDATED) != 1 ||
		countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREDENTIALS_REPLACED) != 1 ||
		countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREATED) != 4 {
		t.Fatal("audit events")
	}

	// Key rotation: the re-encryption job reseals every credential.
	newKey, _ := secrets.GenerateKey("next")
	ring, _ := secrets.NewKeyring(append([]secrets.Key{newKey}, keyOf(t, w)...)...)
	changed, err := w.l.Integrations.Reencrypt(ctx, ring)
	if err != nil || changed != 7 {
		t.Fatalf("resealed %d, %v", changed, err)
	}
	var left int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_integration_credentials WHERE key_id <> 'next'").Scan(&left)
	if left != 0 {
		t.Fatalf("%d credentials keep the old key", left)
	}
	if again, err := w.l.Integrations.Reencrypt(ctx, ring); err != nil || again != 0 {
		t.Fatalf("second run %d %v", again, err)
	}

}

func equalIntegration(a, b *tidev1.AgentIntegration) bool {
	return proto.Equal(a, b)
}

func TestAgentIntegrationReadSnapshot(t *testing.T) {
	for _, mutation := range []string{"update", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			w := newWorld(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			admin := w.env.User(w.org.Admin).Integration
			created, err := admin.CreateAgentIntegration(ctx, connect.NewRequest(&tidev1.CreateAgentIntegrationRequest{
				OrganizationId: w.org.ID, Vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
				Settings: &tidev1.CreateAgentIntegrationRequest_Devin{Devin: &tidev1.DevinSettings{OutpostName: "original"}},
				Credentials: &tidev1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &tidev1.DevinCredentials{
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
			get := func() (*tidev1.AgentIntegration, error) {
				res, err := member.GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{
					OrganizationId: w.org.ID, AgentIntegrationId: id,
				}))
				if err != nil {
					return nil, err
				}
				return res.Msg.GetIntegration(), nil
			}
			list := func() ([]*tidev1.AgentIntegration, error) {
				res, err := member.ListAgentIntegrations(ctx, connect.NewRequest(&tidev1.ListAgentIntegrationsRequest{
					OrganizationId: w.org.ID, PageSize: 1,
				}))
				if err != nil {
					return nil, err
				}
				return res.Msg.GetIntegrations(), nil
			}
			var (
				gotIntegration  *tidev1.AgentIntegration
				gotPage         []*tidev1.AgentIntegration
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
			if listErr != nil || !slices.EqualFunc(gotPage, []*tidev1.AgentIntegration{before}, equalIntegration) {
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
			if listErr != nil || !slices.EqualFunc(afterList, []*tidev1.AgentIntegration{expected}, equalIntegration) {
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
	w := newWorld(t)
	ctx := context.Background()
	resp, err := w.env.User(w.org.Admin).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&tidev1.CreateAgentIntegrationRequest{
		OrganizationId: w.org.ID, Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "cursor-key"}}}))
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
		name, hold, read string
		arg              string
		run              func() (int64, error)
	}{
		{"credential", "UPDATE agent_integration_credentials SET ciphertext = $2 WHERE agent_integration_id = $1",
			"SELECT key_id, ciphertext FROM agent_integration_credentials WHERE agent_integration_id = $1", integrationID,
			func() (int64, error) { return w.l.Integrations.Reencrypt(ctx, ring) }},
		{"ownsand link", "UPDATE ownsand_links SET api_key_ciphertext = $2 WHERE organization_id = $1",
			"SELECT api_key_key_id, api_key_ciphertext FROM ownsand_links WHERE organization_id = $1", w.org.ID,
			func() (int64, error) { return w.l.Links.Reencrypt(ctx, ring) }},
	} {
		// The held update stands in for a replacement that commits after
		// the job's scan.
		commit := holdTx(t, w.env, tc.hold, tc.arg, replaced)
		type result struct {
			n   int64
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := tc.run()
			done <- result{n, err}
		}()
		waitForLockWaiter(t, w.env)
		commit()
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
