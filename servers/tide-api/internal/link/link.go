// SPDX-License-Identifier: FSL-1.1-ALv2

// Package link implements OwnSandLinkService and gives the other layers the
// OwnSand client of an organization. The owning contract is the OwnSand link
// section of docs/servers-tide-api-foundation.md.
//
// The OwnSand API key is stored encrypted (package secrets) with the
// associated data AssociatedData("ownsand_link", <organization ID>). A call
// that OwnSand rejects with unauthenticated marks the link INVALID, which
// stops dispatch for the organization until an admin sets a new key.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
)

// ownsandAPIKeyPrefix is the prefix of OwnSand organization API keys.
const ownsandAPIKeyPrefix = "osk_"

// maxAPIKeyLength bounds the stored key; an OwnSand key has 47 characters.
const maxAPIKeyLength = 256

// Link is a stored OwnSand link without the key.
type Link struct {
	LinkID                string
	OrganizationID        string
	OwnSandOrganizationID string
	State                 tidev1.OwnSandLinkState
	Removing              bool
	CredentialSetTime     time.Time
	CredentialRevision    string
	CreateTime            time.Time
}

// Dispatchable reports whether the dispatcher may create runners for the
// organization: the link is ACTIVE and no removal is in progress.
func (l Link) Dispatchable() bool {
	return l.State == tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE && !l.Removing
}

func (l Link) proto() *tidev1.OwnSandLink {
	return &tidev1.OwnSandLink{
		OrganizationId: l.OrganizationID, OwnsandOrganizationId: l.OwnSandOrganizationID, State: l.State,
		CredentialSet: true, CredentialSetAt: api.Timestamp(l.CredentialSetTime), CreateTime: api.Timestamp(l.CreateTime),
	}
}

// RemovalHook lets a later layer clean up when RemoveOwnSandLink runs. When
// the hooks run, the link is marked as being removed, so the dispatcher
// creates no new runner. Prepare runs outside a transaction and may call
// OwnSand with Links.ClientForRemoval (for example, to delete the runners'
// sandboxes); an error stops the removal and keeps the link. InTx runs in
// the transaction that deletes the link, after the organization row lock.
type RemovalHook struct {
	Name    string
	Prepare func(ctx context.Context, orgID string) error
	InTx    func(ctx context.Context, tx pgx.Tx, orgID string) error
	// AfterCommit runs after the link is deleted (best effort).
	AfterCommit func(ctx context.Context, orgID string)
}

// Links reads links and builds the OwnSand clients of organizations.
type Links struct {
	svc     *api.Service
	factory ownsandclient.Factory
	metrics *metrics.Metrics
	removal RemovalOptions
	hooks   []RemovalHook

	mu      sync.Mutex
	clients map[string]cachedClient
}

type cachedClient struct {
	revision string
	client   ownsandclient.Client
}

// Register mounts OwnSandLinkService and its daily re-encryption job and
// returns the Links of the other layers.
func Register(r *app.Registry, factory ownsandclient.Factory, m *metrics.Metrics, removal RemovalOptions) *Links {
	if removal.LeaseDuration <= 0 {
		removal.LeaseDuration = time.Minute
	}
	l := &Links{svc: r.Service, factory: factory, metrics: m, removal: removal, clients: map[string]cachedClient{}}
	r.Handle(tidev1connect.NewOwnSandLinkServiceHandler(&Handler{links: l}, r.HandlerOptions()...))
	r.AddDailyJob(jobs.Job{Name: "reencrypt_ownsand_links", Run: func(ctx context.Context, _ time.Time) (int64, error) { return l.Reencrypt(ctx, l.svc.Secrets) }})
	return l
}

// OnRemoval registers a removal hook. Hooks run in registration order.
func (l *Links) OnRemoval(h RemovalHook) { l.hooks = append(l.hooks, h) }

// Get returns the link of an organization; ok is false without a link.
func (l *Links) Get(ctx context.Context, q db.Querier, orgID string) (Link, bool, error) {
	var lk Link
	var state int16
	err := q.QueryRow(ctx, `SELECT ownsand_link_id::text, organization_id::text, ownsand_organization_id::text, state, removing, credential_set_time, create_time, credential_revision::text
		FROM ownsand_links WHERE organization_id = $1`, orgID).Scan(&lk.LinkID, &lk.OrganizationID, &lk.OwnSandOrganizationID, &state, &lk.Removing, &lk.CredentialSetTime, &lk.CreateTime, &lk.CredentialRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	lk.State = tidev1.OwnSandLinkState(state)
	return lk, true, nil
}

func associatedData(orgID string) []byte { return secrets.AssociatedData("ownsand_link", orgID) }

// Client returns the OwnSand client of an organization with an ACTIVE link.
// Without a link, or with an INVALID one, it fails with failed_precondition.
func (l *Links) Client(ctx context.Context, orgID string) (ownsandclient.Client, Link, error) {
	return l.client(ctx, orgID, false)
}

// ClientForRemoval returns the client also while the link is being removed
// or INVALID, for clean-up calls (they fail with unauthenticated when the key
// is no longer valid).
func (l *Links) ClientForRemoval(ctx context.Context, orgID string) (ownsandclient.Client, Link, error) {
	return l.client(ctx, orgID, true)
}

func (l *Links) client(ctx context.Context, orgID string, any bool) (ownsandclient.Client, Link, error) {
	s := l.svc
	var lk Link
	var state int16
	var keyID string
	var ciphertext []byte
	err := s.DB.Pool.QueryRow(ctx, `SELECT ownsand_organization_id::text, state, removing, credential_set_time, create_time, credential_revision::text, api_key_key_id, api_key_ciphertext
		FROM ownsand_links WHERE organization_id = $1`, orgID).Scan(&lk.OwnSandOrganizationID, &state, &lk.Removing, &lk.CredentialSetTime, &lk.CreateTime, &lk.CredentialRevision, &keyID, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, Link{}, api.ErrPrecondition("the organization has no OwnSand link")
	}
	if err != nil {
		return nil, Link{}, err
	}
	lk.OrganizationID = orgID
	lk.State = tidev1.OwnSandLinkState(state)
	if !any && lk.State != tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE {
		return nil, lk, api.ErrPrecondition("the OwnSand link is invalid; an admin must set a new OwnSand API key")
	}
	l.mu.Lock()
	cached, ok := l.clients[orgID]
	l.mu.Unlock()
	if ok && cached.revision == lk.CredentialRevision {
		return cached.client, lk, nil
	}
	key, err := s.Secrets.Open(secrets.Sealed{KeyID: keyID, Ciphertext: ciphertext}, associatedData(orgID))
	if err != nil {
		return nil, lk, fmt.Errorf("decrypt the OwnSand API key: %w", err)
	}
	inner, err := l.factory.New(lk.OwnSandOrganizationID, string(key))
	clear(key)
	if err != nil {
		return nil, lk, err
	}
	revision := lk.CredentialRevision
	c := l.instrument(orgID, inner, func() { l.markInvalid(orgID, revision) })
	l.mu.Lock()
	l.clients[orgID] = cachedClient{revision: revision, client: c}
	l.mu.Unlock()
	return c, lk, nil
}

func (l *Links) instrument(orgID string, inner ownsandclient.Client, onUnauthenticated func()) ownsandclient.Client {
	return &ownsandclient.Instrumented{Inner: inner, Metrics: l.metrics, Logger: l.svc.Logger, OrganizationID: orgID, OnUnauthenticated: onUnauthenticated}
}

// markInvalid marks the link INVALID when the key that failed is still the
// stored key, so a key that an admin set meanwhile stays ACTIVE.
func (l *Links) markInvalid(orgID, revision string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, err := l.svc.DB.Pool.Exec(ctx, `UPDATE ownsand_links SET state = $3 WHERE organization_id = $1 AND credential_revision = $2 AND state = $4`,
		orgID, revision, int16(tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_INVALID), int16(tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE))
	if err != nil {
		l.svc.Logger.Warn("ownsand_link_invalidate_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		return
	}
	if tag.RowsAffected() > 0 {
		l.svc.Logger.Warn("ownsand_link_invalid", slog.String("organization_id", orgID),
			slog.String("detail", "OwnSand rejected the API key; dispatch stops until an admin sets a new key"))
	}
}

func (l *Links) forget(orgID string) {
	l.mu.Lock()
	delete(l.clients, orgID)
	l.mu.Unlock()
}

// Reencrypt reseals the stored keys whose key is not the primary key of
// ring. The daily job calls it with the service keyring.
func (l *Links) Reencrypt(ctx context.Context, ring *secrets.Keyring) (int64, error) {
	s := l.svc
	rows, err := s.DB.Pool.Query(ctx, "SELECT organization_id::text, api_key_key_id, api_key_ciphertext FROM ownsand_links WHERE api_key_key_id <> $1", ring.PrimaryKeyID())
	if err != nil {
		return 0, err
	}
	type row struct {
		org    string
		sealed secrets.Sealed
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.org, &r.sealed.KeyID, &r.sealed.Ciphertext); err != nil {
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
		out, changed, err := ring.Reseal(r.sealed, associatedData(r.org))
		if err != nil {
			s.Logger.WarnContext(ctx, "reencrypt_failed", slog.String("table", "ownsand_links"), slog.String("organization_id", r.org), slog.Any("error", err))
			continue
		}
		if !changed {
			continue
		}
		// The ciphertext condition skips a key that was replaced after the
		// scan: a replacement on an instance that still has the old primary
		// key keeps api_key_key_id, and its new value must stay.
		tag, err := s.DB.Pool.Exec(ctx, "UPDATE ownsand_links SET api_key_key_id = $2, api_key_ciphertext = $3 WHERE organization_id = $1 AND api_key_key_id = $4 AND api_key_ciphertext = $5",
			r.org, out.KeyID, out.Ciphertext, r.sealed.KeyID, r.sealed.Ciphertext)
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

// Handler serves OwnSandLinkService.
type Handler struct{ links *Links }

var _ tidev1connect.OwnSandLinkServiceHandler = (*Handler)(nil)

func validateAPIKey(key string) error {
	if !strings.HasPrefix(key, ownsandAPIKeyPrefix) || len(key) > maxAPIKeyLength || len(key) <= len(ownsandAPIKeyPrefix) {
		return api.ErrInvalidf("invalid ownsand_api_key: must be an OwnSand organization API key (osk_...)")
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] >= 0x7f {
			return api.ErrInvalidf("invalid ownsand_api_key: must be an OwnSand organization API key (osk_...)")
		}
	}
	return nil
}

// SetOwnSandLink validates the key with OwnSand ListDevices before it stores
// it, so a stored link always matches its key.
func (h *Handler) SetOwnSandLink(ctx context.Context, req *connect.Request[tidev1.SetOwnSandLinkRequest]) (*connect.Response[tidev1.SetOwnSandLinkResponse], error) {
	l := h.links
	s := l.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageIntegrations)
	if err != nil {
		return nil, err
	}
	ownsandOrg, err := api.NormalizeID("ownsand_organization_id", req.Msg.GetOwnsandOrganizationId())
	if err != nil {
		return nil, err
	}
	key := req.Msg.GetOwnsandApiKey()
	if err := validateAPIKey(key); err != nil {
		return nil, err
	}
	existing, ok, err := l.Get(ctx, s.DB.Pool, c.OrganizationID)
	if err != nil {
		return nil, s.Internal(ctx, "load ownsand link", err)
	}
	if ok && existing.OwnSandOrganizationID != ownsandOrg {
		return nil, api.ErrPrecondition("the organization is linked to another OwnSand organization; remove the link first")
	}
	if ok && existing.Removing {
		return nil, api.ErrPrecondition("the OwnSand link is being removed")
	}
	if err := s.AllowOwnSandLinkValidation(ctx, c.UserID); err != nil {
		return nil, err
	}
	// The validation client is not cached and does not mark the stored link
	// invalid: a wrong new key says nothing about the stored one.
	inner, err := l.factory.New(ownsandOrg, key)
	if err != nil {
		return nil, s.Internal(ctx, "build ownsand client", err)
	}
	if _, err := l.instrument(c.OrganizationID, inner, nil).ListDevices(ctx); err != nil {
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated:
			return nil, api.ErrInvalidf("invalid ownsand_api_key: OwnSand rejected the key")
		case connect.CodeNotFound, connect.CodePermissionDenied:
			return nil, api.ErrInvalidf("invalid ownsand_api_key: the key does not belong to OwnSand organization %s", ownsandOrg)
		case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
			return nil, api.ErrUnavailable("OwnSand is unavailable; try again")
		}
		return nil, s.Internal(ctx, "validate ownsand link", err)
	}
	sealed, err := s.Secrets.Seal([]byte(key), associatedData(c.OrganizationID))
	if err != nil {
		return nil, s.Internal(ctx, "encrypt ownsand api key", err)
	}
	now := s.NowUTC()
	revision := api.NewID()
	var out Link
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ownsand_links (ownsand_link_id, organization_id, ownsand_organization_id, state, api_key_key_id, api_key_ciphertext, credential_set_time, create_time, credential_revision)
			VALUES ($7, $1, $2, $3, $4, $5, $6, $6, $8)
			ON CONFLICT (organization_id) DO UPDATE SET state = EXCLUDED.state, api_key_key_id = EXCLUDED.api_key_key_id,
				api_key_ciphertext = EXCLUDED.api_key_ciphertext, credential_set_time = EXCLUDED.credential_set_time,
				credential_revision = EXCLUDED.credential_revision
			WHERE ownsand_links.ownsand_organization_id = EXCLUDED.ownsand_organization_id AND NOT ownsand_links.removing`,
			c.OrganizationID, ownsandOrg, int16(tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE), sealed.KeyID, sealed.Ciphertext, now, api.NewID(), revision)
		if db.IsUniqueViolation(err) {
			return api.ErrAlreadyExists("another tide organization links this OwnSand organization")
		}
		if err != nil {
			return err
		}
		lk, ok, err := l.Get(ctx, tx, c.OrganizationID)
		if err != nil {
			return err
		}
		if !ok || lk.OwnSandOrganizationID != ownsandOrg || lk.CredentialRevision != revision {
			return api.ErrPrecondition("the OwnSand link changed meanwhile; try again")
		}
		out = lk
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_SET, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_OWNSAND_LINK, TargetID: lk.LinkID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "set ownsand link", err)
	}
	l.forget(c.OrganizationID)
	s.Logger.InfoContext(ctx, "ownsand_link_set", slog.String("organization_id", c.OrganizationID), slog.String("ownsand_organization_id", ownsandOrg))
	s.WorkChanged(c.OrganizationID)
	return connect.NewResponse(&tidev1.SetOwnSandLinkResponse{Link: out.proto()}), nil
}

func (h *Handler) GetOwnSandLink(ctx context.Context, req *connect.Request[tidev1.GetOwnSandLinkRequest]) (*connect.Response[tidev1.GetOwnSandLinkResponse], error) {
	s := h.links.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	lk, ok, err := h.links.Get(ctx, s.DB.Pool, c.OrganizationID)
	if err != nil {
		return nil, s.Internal(ctx, "get ownsand link", err)
	}
	resp := &tidev1.GetOwnSandLinkResponse{}
	if ok {
		resp.Link = lk.proto()
	}
	return connect.NewResponse(resp), nil
}

// RemoveOwnSandLink marks the link as being removed (dispatch stops), runs
// the Prepare hooks (the work layer deletes the runners), then deletes the
// link and its key in a transaction that also runs the InTx hooks (the work
// layer ends the queued requests with CANCELED). On failure the handler
// keeps the link and tries to release its claim.
func (h *Handler) RemoveOwnSandLink(ctx context.Context, req *connect.Request[tidev1.RemoveOwnSandLinkRequest]) (*connect.Response[tidev1.RemoveOwnSandLinkResponse], error) {
	l := h.links
	s := l.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageIntegrations)
	if err != nil {
		return nil, err
	}
	claim, err := l.ClaimRemoval(ctx, c.OrganizationID)
	if err != nil {
		return nil, s.Internal(ctx, "claim ownsand link removal", err)
	}
	if !claim.HasLink() {
		return nil, api.ErrNotFound("OwnSand link")
	}

	committed := false
	cleanupCtx, stop := l.WatchRemoval(ctx, claim)
	defer func() {
		if !committed {
			stop()
			l.ReleaseRemoval(ctx, claim)
		}
	}()
	for _, hook := range l.hooks {
		if hook.Prepare == nil {
			continue
		}
		if err := api.CheckTeardown(cleanupCtx); err != nil {
			return nil, s.Internal(ctx, "check ownsand link removal", err)
		}
		if err := hook.Prepare(cleanupCtx, c.OrganizationID); err != nil {
			s.Logger.WarnContext(ctx, "ownsand_link_removal_prepare_failed", slog.String("hook", hook.Name), slog.String("organization_id", c.OrganizationID), slog.Any("error", err))
			return nil, s.Internal(ctx, "prepare ownsand link removal", err)
		}
	}
	err = s.DB.InTx(cleanupCtx, func(tx pgx.Tx) error {
		ctx := cleanupCtx
		if err := api.LockOrganization(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		if err := l.CheckRemoval(ctx, tx, claim); err != nil {
			return err
		}
		for _, hook := range l.hooks {
			if hook.InTx == nil {
				continue
			}
			if err := hook.InTx(ctx, tx, c.OrganizationID); err != nil {
				return fmt.Errorf("ownsand link removal hook %s: %w", hook.Name, err)
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM ownsand_links WHERE ownsand_link_id = $1", claim.linkID); err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_REMOVED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_OWNSAND_LINK, TargetID: claim.linkID,
		})
	})
	if err != nil {
		s.Logger.WarnContext(ctx, "ownsand_link_removal_failed", slog.String("organization_id", c.OrganizationID), slog.Any("error", err))
		return nil, s.Internal(ctx, "remove ownsand link", err)
	}
	committed = true
	stop()
	l.forget(c.OrganizationID)
	for _, hook := range l.hooks {
		if hook.AfterCommit != nil {
			hook.AfterCommit(context.WithoutCancel(ctx), c.OrganizationID)
		}
	}
	s.Logger.InfoContext(ctx, "ownsand_link_removed", slog.String("organization_id", c.OrganizationID))
	return connect.NewResponse(&tidev1.RemoveOwnSandLinkResponse{}), nil
}
