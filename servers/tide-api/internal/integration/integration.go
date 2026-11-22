// SPDX-License-Identifier: FSL-1.1-ALv2

// Package integration implements AgentIntegrationService: the vendor
// settings and credentials of agent integrations. The owning contracts are
// the Agent integrations and Secrets sections of
// docs/servers-tide-api-foundation.md and the agent integration messages of
// docs/protos-tide-api-contract.md.
//
// Each credential is stored encrypted (package secrets) in
// agent_integration_credentials with the associated data
// AssociatedData("agent_integration", <integration ID>, <credential number>).
// The API accepts credentials only in create and update requests and returns
// only whether each one is set and when. Vendor layers read a credential with
// Store.Credential right before the call or the runner that uses it.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/jobs"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
)

// Limits of the protos contract.
const (
	maxNameLength       = 64
	maxSettingLength    = 200
	maxCredentialLength = 8 << 10
)

// Credentials returns the credentials of each vendor, in response order.
func Credentials(v tidev1.AgentVendor) []tidev1.AgentCredential {
	switch v {
	case tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		return []tidev1.AgentCredential{tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY}
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR:
		return []tidev1.AgentCredential{tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY}
	case tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		return []tidev1.AgentCredential{tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, tidev1.AgentCredential_AGENT_CREDENTIAL_WEBHOOK_SECRET}
	case tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		return []tidev1.AgentCredential{tidev1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN, tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN}
	}
	return nil
}

// Integration is a stored integration without credentials.
type Integration struct {
	ID             string
	OrganizationID string
	Vendor         tidev1.AgentVendor
	Name           string
	PoolName       string
	OutpostName    string
	CreateTime     time.Time
	UpdateTime     time.Time
}

// Store reads integrations and their credentials.
type Store struct {
	svc            *api.Service
	publicOrigin   string
	deletionChecks []DeletionCheck
}

// DeletionCheck can refuse the deletion of an integration. It runs in the
// deletion transaction after the integration row is locked, and refuses
// with an error such as api.ErrPrecondition.
type DeletionCheck func(ctx context.Context, tx pgx.Tx, orgID, integrationID string) error

// OnDeletion adds a deletion check. Call it before the app runs.
func (st *Store) OnDeletion(check DeletionCheck) {
	st.deletionChecks = append(st.deletionChecks, check)
}

// Register mounts AgentIntegrationService and its daily re-encryption job.
// publicOrigin is the public origin of tide-api for the OPENAI_CODEX
// webhook URL.
func Register(r *app.Registry, publicOrigin string) *Store {
	st := &Store{svc: r.Service, publicOrigin: publicOrigin}
	r.Handle(tidev1connect.NewAgentIntegrationServiceHandler(&Handler{st: st}, r.HandlerOptions()...))
	r.AddDailyJob(jobs.Job{Name: "reencrypt_agent_credentials", Run: func(ctx context.Context, _ time.Time) (int64, error) { return st.Reencrypt(ctx, st.svc.Secrets) }})
	return st
}

// WebhookURL returns the OpenAI webhook URL of an OPENAI_CODEX integration.
func (st *Store) WebhookURL(integrationID string) string {
	return st.publicOrigin + "/webhooks/openai/" + integrationID
}

const integrationColumns = `agent_integration_id::text, organization_id::text, vendor, name, pool_name, outpost_name, create_time, update_time`

func scanIntegration(row pgx.Row) (Integration, error) {
	var in Integration
	var vendor int16
	err := row.Scan(&in.ID, &in.OrganizationID, &vendor, &in.Name, &in.PoolName, &in.OutpostName, &in.CreateTime, &in.UpdateTime)
	in.Vendor = tidev1.AgentVendor(vendor)
	return in, err
}

// Get loads an integration of the organization; it fails with not_found for
// an unknown ID or an integration of another organization.
func (st *Store) Get(ctx context.Context, q db.Querier, orgID, id string) (Integration, error) {
	in, err := scanIntegration(q.QueryRow(ctx, "SELECT "+integrationColumns+" FROM agent_integrations WHERE organization_id = $1 AND agent_integration_id = $2", orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Integration{}, api.ErrNotFound("agent integration")
	}
	return in, err
}

func associatedData(integrationID string, cred tidev1.AgentCredential) []byte {
	return secrets.AssociatedData("agent_integration", integrationID, strconv.Itoa(int(cred)))
}

// Credential decrypts one credential of an integration. Callers decrypt a
// credential only for the call that uses it and never log it.
func (st *Store) Credential(ctx context.Context, orgID, integrationID string, cred tidev1.AgentCredential) (string, error) {
	var sealed secrets.Sealed
	err := st.svc.DB.Pool.QueryRow(ctx, `SELECT c.key_id, c.ciphertext FROM agent_integration_credentials c
		JOIN agent_integrations i ON i.agent_integration_id = c.agent_integration_id
		WHERE i.organization_id = $1 AND c.agent_integration_id = $2 AND c.credential = $3`, orgID, integrationID, int16(cred)).Scan(&sealed.KeyID, &sealed.Ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", api.ErrNotFound("agent integration credential")
	}
	if err != nil {
		return "", err
	}
	plain, err := st.svc.Secrets.Open(sealed, associatedData(integrationID, cred))
	if err != nil {
		return "", fmt.Errorf("decrypt agent credential: %w", err)
	}
	return string(plain), nil
}

func (st *Store) proto(ctx context.Context, q db.Querier, in Integration) (*tidev1.AgentIntegration, error) {
	out := &tidev1.AgentIntegration{
		AgentIntegrationId: in.ID, OrganizationId: in.OrganizationID, Vendor: in.Vendor, Name: in.Name,
		CreateTime: api.Timestamp(in.CreateTime), UpdateTime: api.Timestamp(in.UpdateTime),
	}
	switch in.Vendor {
	case tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		out.Settings = &tidev1.AgentIntegration_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}}
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR:
		out.Settings = &tidev1.AgentIntegration_Cursor{Cursor: &tidev1.CursorSettings{PoolName: in.PoolName}}
	case tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		out.Settings = &tidev1.AgentIntegration_OpenaiCodex{OpenaiCodex: &tidev1.OpenAiCodexSettings{}}
		out.WebhookUrl = st.WebhookURL(in.ID)
	case tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		out.Settings = &tidev1.AgentIntegration_Devin{Devin: &tidev1.DevinSettings{OutpostName: in.OutpostName}}
	}
	set := map[tidev1.AgentCredential]time.Time{}
	rows, err := q.Query(ctx, "SELECT credential, set_time FROM agent_integration_credentials WHERE agent_integration_id = $1", in.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c int16
		var t time.Time
		if err := rows.Scan(&c, &t); err != nil {
			rows.Close()
			return nil, err
		}
		set[tidev1.AgentCredential(c)] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range Credentials(in.Vendor) {
		state := &tidev1.AgentCredentialState{Credential: c}
		if t, ok := set[c]; ok {
			state.CredentialSet = true
			state.CredentialSetAt = api.Timestamp(t)
		}
		out.Credentials = append(out.Credentials, state)
	}
	return out, nil
}

// settingsValue is the vendor of a settings oneof and its values.
type settingsValue struct {
	vendor                tidev1.AgentVendor
	poolName, outpostName string
}

func settingsFrom(v any) (settingsValue, bool) {
	switch s := v.(type) {
	case *tidev1.CreateAgentIntegrationRequest_ClaudeCode, *tidev1.UpdateAgentIntegrationRequest_ClaudeCode:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE}, true
	case *tidev1.CreateAgentIntegrationRequest_OpenaiCodex, *tidev1.UpdateAgentIntegrationRequest_OpenaiCodex:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX}, true
	case *tidev1.CreateAgentIntegrationRequest_Cursor:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, poolName: s.Cursor.GetPoolName()}, true
	case *tidev1.UpdateAgentIntegrationRequest_Cursor:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, poolName: s.Cursor.GetPoolName()}, true
	case *tidev1.CreateAgentIntegrationRequest_Devin:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, outpostName: s.Devin.GetOutpostName()}, true
	case *tidev1.UpdateAgentIntegrationRequest_Devin:
		return settingsValue{vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, outpostName: s.Devin.GetOutpostName()}, true
	}
	return settingsValue{}, false
}

func (sv settingsValue) validate() error {
	check := func(field, v string) error {
		if err := api.ValidateName(field, v, maxSettingLength); err != nil {
			return err
		}
		return nil
	}
	switch sv.vendor {
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR:
		return check("settings.cursor.pool_name", sv.poolName)
	case tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		return check("settings.devin.outpost_name", sv.outpostName)
	}
	return nil
}

// credentialsFrom returns the vendor and the non-empty credential values of a
// credentials oneof.
func credentialsFrom(v any) (tidev1.AgentVendor, map[tidev1.AgentCredential]string, bool) {
	m := map[tidev1.AgentCredential]string{}
	put := func(c tidev1.AgentCredential, value string) {
		if value != "" {
			m[c] = value
		}
	}
	switch c := v.(type) {
	case *tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, c.ClaudeCodeCredentials.GetEnvironmentKey())
		return tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, m, true
	case *tidev1.UpdateAgentIntegrationRequest_ClaudeCodeCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, c.ClaudeCodeCredentials.GetEnvironmentKey())
		return tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, m, true
	case *tidev1.CreateAgentIntegrationRequest_CursorCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY, c.CursorCredentials.GetServiceAccountApiKey())
		return tidev1.AgentVendor_AGENT_VENDOR_CURSOR, m, true
	case *tidev1.UpdateAgentIntegrationRequest_CursorCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY, c.CursorCredentials.GetServiceAccountApiKey())
		return tidev1.AgentVendor_AGENT_VENDOR_CURSOR, m, true
	case *tidev1.CreateAgentIntegrationRequest_OpenaiCodexCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, c.OpenaiCodexCredentials.GetEnvironmentKey())
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_WEBHOOK_SECRET, c.OpenaiCodexCredentials.GetWebhookSecret())
		return tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, m, true
	case *tidev1.UpdateAgentIntegrationRequest_OpenaiCodexCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY, c.OpenaiCodexCredentials.GetEnvironmentKey())
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_WEBHOOK_SECRET, c.OpenaiCodexCredentials.GetWebhookSecret())
		return tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, m, true
	case *tidev1.CreateAgentIntegrationRequest_DevinCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN, c.DevinCredentials.GetApiToken())
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN, c.DevinCredentials.GetOutpostToken())
		return tidev1.AgentVendor_AGENT_VENDOR_DEVIN, m, true
	case *tidev1.UpdateAgentIntegrationRequest_DevinCredentials:
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN, c.DevinCredentials.GetApiToken())
		put(tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN, c.DevinCredentials.GetOutpostToken())
		return tidev1.AgentVendor_AGENT_VENDOR_DEVIN, m, true
	}
	return 0, nil, false
}

func validateCredentialValues(values map[tidev1.AgentCredential]string) error {
	for c, v := range values {
		if len(v) > maxCredentialLength {
			return api.ErrInvalidf("invalid credentials: %s must be at most %d bytes", c, maxCredentialLength)
		}
		for i := 0; i < len(v); i++ {
			if v[i] < ' ' || v[i] == 0x7f {
				return api.ErrInvalidf("invalid credentials: %s must not contain control characters", c)
			}
		}
	}
	return nil
}

// storeCredentials encrypts and upserts credential values.
func (st *Store) storeCredentials(ctx context.Context, tx pgx.Tx, integrationID string, values map[tidev1.AgentCredential]string, now time.Time) error {
	for c, v := range values {
		sealed, err := st.svc.Secrets.Seal([]byte(v), associatedData(integrationID, c))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_integration_credentials (agent_integration_id, credential, key_id, ciphertext, set_time)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (agent_integration_id, credential) DO UPDATE
			SET key_id = EXCLUDED.key_id, ciphertext = EXCLUDED.ciphertext, set_time = EXCLUDED.set_time`,
			integrationID, int16(c), sealed.KeyID, sealed.Ciphertext, now); err != nil {
			return err
		}
	}
	return nil
}

// Reencrypt reseals the credentials whose key is not the primary key of
// ring. The daily job calls it with the service keyring.
func (st *Store) Reencrypt(ctx context.Context, ring *secrets.Keyring) (int64, error) {
	s := st.svc
	rows, err := s.DB.Pool.Query(ctx, "SELECT agent_integration_id::text, credential, key_id, ciphertext FROM agent_integration_credentials WHERE key_id <> $1", ring.PrimaryKeyID())
	if err != nil {
		return 0, err
	}
	type row struct {
		id     string
		cred   int16
		sealed secrets.Sealed
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.cred, &r.sealed.KeyID, &r.sealed.Ciphertext); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, r := range todo {
		out, changed, err := ring.Reseal(r.sealed, associatedData(r.id, tidev1.AgentCredential(r.cred)))
		if err != nil {
			s.Logger.WarnContext(ctx, "reencrypt_failed", slog.String("table", "agent_integration_credentials"), slog.String("agent_integration_id", r.id), slog.Any("error", err))
			continue
		}
		if !changed {
			continue
		}
		// The ciphertext condition skips a credential that was replaced
		// after the scan: a replacement on an instance that still has the
		// old primary key keeps key_id, and its new value must stay.
		tag, err := s.DB.Pool.Exec(ctx, `UPDATE agent_integration_credentials SET key_id = $3, ciphertext = $4
			WHERE agent_integration_id = $1 AND credential = $2 AND key_id = $5 AND ciphertext = $6`, r.id, r.cred, out.KeyID, out.Ciphertext, r.sealed.KeyID, r.sealed.Ciphertext)
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

// Handler serves AgentIntegrationService.
type Handler struct{ st *Store }

var _ tidev1connect.AgentIntegrationServiceHandler = (*Handler)(nil)

func (h *Handler) CreateAgentIntegration(ctx context.Context, req *connect.Request[tidev1.CreateAgentIntegrationRequest]) (*connect.Response[tidev1.CreateAgentIntegrationResponse], error) {
	st := h.st
	s := st.svc
	m := req.Msg
	var resp *tidev1.CreateAgentIntegrationResponse
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, m.GetOrganizationId(), authz.ActionManageIntegrations)
		if err != nil {
			return err
		}
		vendor := m.GetVendor()
		if len(Credentials(vendor)) == 0 {
			return api.ErrInvalidf("invalid vendor: must be one of CLAUDE_CODE, CURSOR, OPENAI_CODEX and DEVIN")
		}
		if err := api.ValidateName("name", m.GetName(), maxNameLength); err != nil {
			return err
		}
		sv, ok := settingsFrom(m.GetSettings())
		if !ok || sv.vendor != vendor {
			return api.ErrInvalidf("invalid settings: must be the settings of %s", vendor)
		}
		if err := sv.validate(); err != nil {
			return err
		}
		credVendor, values, ok := credentialsFrom(m.GetCredentials())
		if !ok || credVendor != vendor {
			return api.ErrInvalidf("invalid credentials: must be the credentials of %s", vendor)
		}
		for _, cred := range Credentials(vendor) {
			if values[cred] == "" {
				return api.ErrInvalidf("invalid credentials: %s is required", cred)
			}
		}
		if err := validateCredentialValues(values); err != nil {
			return err
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		id := api.NewID()
		now := s.NowUTC()
		in, err := scanIntegration(tx.QueryRow(ctx, `INSERT INTO agent_integrations (agent_integration_id, organization_id, vendor, name, pool_name, outpost_name, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+integrationColumns, id, c.OrganizationID, int16(vendor), m.GetName(), sv.poolName, sv.outpostName, now))
		if db.IsUniqueViolation(err) {
			return api.ErrAlreadyExists("an agent integration with this name exists")
		}
		if err != nil {
			return err
		}
		if err := st.storeCredentials(ctx, tx, id, values, now); err != nil {
			return err
		}
		if err := s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_INTEGRATION, TargetID: id,
		}); err != nil {
			return err
		}
		p, err := st.proto(ctx, tx, in)
		resp = &tidev1.CreateAgentIntegrationResponse{Integration: p}
		return err
	})
	if err != nil {
		return nil, s.Internal(ctx, "create agent integration", err)
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) GetAgentIntegration(ctx context.Context, req *connect.Request[tidev1.GetAgentIntegrationRequest]) (*connect.Response[tidev1.GetAgentIntegrationResponse], error) {
	s := h.st.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("agent_integration_id", req.Msg.GetAgentIntegrationId())
	if err != nil {
		return nil, err
	}
	in, err := h.st.Get(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, s.Internal(ctx, "get agent integration", err)
	}
	p, err := h.st.proto(ctx, s.DB.Pool, in)
	if err != nil {
		return nil, s.Internal(ctx, "get agent integration", err)
	}
	return connect.NewResponse(&tidev1.GetAgentIntegrationResponse{Integration: p}), nil
}

func (h *Handler) ListAgentIntegrations(ctx context.Context, req *connect.Request[tidev1.ListAgentIntegrationsRequest]) (*connect.Response[tidev1.ListAgentIntegrationsResponse], error) {
	s := h.st.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+integrationColumns+" FROM agent_integrations WHERE organization_id = $1 AND agent_integration_id > $2 ORDER BY agent_integration_id LIMIT $3",
		c.OrganizationID, api.AfterKey(after), limit)
	if err != nil {
		return nil, s.Internal(ctx, "list agent integrations", err)
	}
	var list []Integration
	for rows.Next() {
		in, err := scanIntegration(rows)
		if err != nil {
			rows.Close()
			return nil, s.Internal(ctx, "list agent integrations", err)
		}
		list = append(list, in)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "list agent integrations", err)
	}
	resp := &tidev1.ListAgentIntegrationsResponse{}
	for _, in := range list {
		p, err := h.st.proto(ctx, s.DB.Pool, in)
		if err != nil {
			return nil, s.Internal(ctx, "list agent integrations", err)
		}
		resp.Integrations = append(resp.Integrations, p)
	}
	last := ""
	if n := len(list); n > 0 {
		last = list[n-1].ID
	}
	resp.NextPageToken = api.NextPageToken(len(list), limit, last)
	return connect.NewResponse(resp), nil
}

func (h *Handler) UpdateAgentIntegration(ctx context.Context, req *connect.Request[tidev1.UpdateAgentIntegrationRequest]) (*connect.Response[tidev1.UpdateAgentIntegrationResponse], error) {
	st := h.st
	s := st.svc
	m := req.Msg
	var resp *tidev1.UpdateAgentIntegrationResponse
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, m.GetOrganizationId(), authz.ActionManageIntegrations)
		if err != nil {
			return err
		}
		id, err := api.NormalizeID("agent_integration_id", m.GetAgentIntegrationId())
		if err != nil {
			return err
		}
		paths := m.GetUpdateMask().GetPaths()
		if len(paths) == 0 {
			return api.ErrInvalidf("invalid update_mask: must name at least one of name, settings and credentials")
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		in, err := scanIntegration(tx.QueryRow(ctx, "SELECT "+integrationColumns+" FROM agent_integrations WHERE organization_id = $1 AND agent_integration_id = $2 FOR UPDATE", c.OrganizationID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return api.ErrNotFound("agent integration")
		}
		if err != nil {
			return err
		}
		var setName, setSettings, setCredentials bool
		for _, p := range paths {
			switch p {
			case "name":
				setName = true
			case "settings":
				setSettings = true
			case "credentials":
				setCredentials = true
			default:
				return api.ErrInvalidf("invalid update_mask: unknown path %q", p)
			}
		}
		now := s.NowUTC()
		if setName {
			if err := api.ValidateName("name", m.GetName(), maxNameLength); err != nil {
				return err
			}
			in.Name = m.GetName()
		}
		if setSettings {
			sv, ok := settingsFrom(m.GetSettings())
			if !ok || sv.vendor != in.Vendor {
				return api.ErrInvalidf("invalid settings: must be the settings of %s", in.Vendor)
			}
			if err := sv.validate(); err != nil {
				return err
			}
			in.PoolName, in.OutpostName = sv.poolName, sv.outpostName
		}
		var values map[tidev1.AgentCredential]string
		if setCredentials {
			vendor, v, ok := credentialsFrom(m.GetCredentials())
			if !ok || vendor != in.Vendor {
				return api.ErrInvalidf("invalid credentials: must be the credentials of %s", in.Vendor)
			}
			if len(v) == 0 {
				return api.ErrInvalidf("invalid credentials: set at least one credential to replace")
			}
			if err := validateCredentialValues(v); err != nil {
				return err
			}
			values = v
		}
		if setName || setSettings {
			_, err := tx.Exec(ctx, "UPDATE agent_integrations SET name = $2, pool_name = $3, outpost_name = $4, update_time = $5 WHERE agent_integration_id = $1",
				id, in.Name, in.PoolName, in.OutpostName, now)
			if db.IsUniqueViolation(err) {
				return api.ErrAlreadyExists("an agent integration with this name exists")
			}
			if err != nil {
				return err
			}
			in.UpdateTime = now
			if err := s.WriteAudit(ctx, tx, api.AuditEvent{
				OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_UPDATED, Actor: c.Principal,
				TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_INTEGRATION, TargetID: id,
			}); err != nil {
				return err
			}
		}
		if values != nil {
			if err := st.storeCredentials(ctx, tx, id, values, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE agent_integrations SET update_time = $2 WHERE agent_integration_id = $1", id, now); err != nil {
				return err
			}
			in.UpdateTime = now
			if err := s.WriteAudit(ctx, tx, api.AuditEvent{
				OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_CREDENTIALS_REPLACED, Actor: c.Principal,
				TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_INTEGRATION, TargetID: id,
			}); err != nil {
				return err
			}
		}
		p, err := st.proto(ctx, tx, in)
		resp = &tidev1.UpdateAgentIntegrationResponse{Integration: p}
		return err
	})
	if err != nil {
		return nil, s.Internal(ctx, "update agent integration", err)
	}
	return connect.NewResponse(resp), nil
}

// DeleteAgentIntegration fails with failed_precondition while a runner
// profile names the integration or a deletion check refuses it. Its
// credentials are deleted with it.
func (h *Handler) DeleteAgentIntegration(ctx context.Context, req *connect.Request[tidev1.DeleteAgentIntegrationRequest]) (*connect.Response[tidev1.DeleteAgentIntegrationResponse], error) {
	s := h.st.svc
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageIntegrations)
		if err != nil {
			return err
		}
		id, err := api.NormalizeID("agent_integration_id", req.Msg.GetAgentIntegrationId())
		if err != nil {
			return err
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		if _, err := scanIntegration(tx.QueryRow(ctx, "SELECT "+integrationColumns+" FROM agent_integrations WHERE organization_id = $1 AND agent_integration_id = $2 FOR UPDATE", c.OrganizationID, id)); errors.Is(err, pgx.ErrNoRows) {
			return api.ErrNotFound("agent integration")
		} else if err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM runner_profiles WHERE organization_id = $1 AND agent_integration_id = $2)", c.OrganizationID, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return api.ErrPrecondition("a runner profile names this agent integration; delete the profile first")
		}
		for _, check := range h.st.deletionChecks {
			if err := check(ctx, tx, c.OrganizationID, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM agent_integrations WHERE agent_integration_id = $1", id); err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_AGENT_INTEGRATION_DELETED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_INTEGRATION, TargetID: id,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "delete agent integration", err)
	}
	return connect.NewResponse(&tidev1.DeleteAgentIntegrationResponse{}), nil
}
