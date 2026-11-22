// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
)

// Handler serves GitHubService.
type Handler struct{ l *Layer }

var _ tidev1connect.GitHubServiceHandler = (*Handler)(nil)

// Input bounds of LinkGitHubInstallation. GitHub OAuth codes are 20
// characters today; the bound leaves room without accepting large values.
const (
	maxCodeLength  = 512
	maxStateLength = 128
)

func hashState(state string) []byte {
	h := sha256.Sum256([]byte(state))
	return h[:]
}

// gitHubUnavailable maps a GitHub call error of an RPC.
func (l *Layer) gitHubUnavailable(ctx context.Context, op string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return l.svc.Internal(ctx, op, err)
	}
	return api.ErrUnavailable("GitHub is unavailable or refused the call; try again")
}

// CreateGitHubInstallUrl returns the installation URL of the app with a new
// single-use state that binds the organization and the caller and expires
// after 10 minutes. Only the SHA-256 hash of the state is stored.
func (h *Handler) CreateGitHubInstallUrl(ctx context.Context, req *connect.Request[tidev1.CreateGitHubInstallUrlRequest]) (*connect.Response[tidev1.CreateGitHubInstallUrlResponse], error) {
	l := h.l
	s := l.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageIntegrations)
	if err != nil {
		return nil, err
	}
	if !l.Configured() {
		return nil, errNotConfigured
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, s.Internal(ctx, "create install state", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	expire := s.NowUTC().Add(installStateLifetime)
	if _, err := s.DB.Pool.Exec(ctx, "INSERT INTO github_install_states (state_hash, organization_id, user_id, expire_time) VALUES ($1, $2, $3, $4)",
		hashState(state), c.OrganizationID, c.UserID, expire); err != nil {
		return nil, s.Internal(ctx, "store install state", err)
	}
	return connect.NewResponse(&tidev1.CreateGitHubInstallUrlResponse{Url: l.gh.InstallURL(state), ExpireTime: api.Timestamp(expire)}), nil
}

// LinkGitHubInstallation consumes the state, exchanges the code for a
// GitHub user access token, checks with GET /user/installations that the
// user can access the installation and that it is on an organization
// account, selects the Default runner group and stores the link. The user
// access token is discarded after the check.
func (h *Handler) LinkGitHubInstallation(ctx context.Context, req *connect.Request[tidev1.LinkGitHubInstallationRequest]) (*connect.Response[tidev1.LinkGitHubInstallationResponse], error) {
	l := h.l
	s := l.svc
	m := req.Msg
	c, err := s.Authorize(ctx, s.DB.Pool, m.GetOrganizationId(), authz.ActionManageIntegrations)
	if err != nil {
		return nil, err
	}
	if !l.Configured() {
		return nil, errNotConfigured
	}
	ghID := m.GetGithubInstallationId()
	switch {
	case ghID <= 0:
		return nil, api.ErrInvalidf("invalid github_installation_id: must be positive")
	case m.GetCode() == "" || len(m.GetCode()) > maxCodeLength:
		return nil, api.ErrInvalidf("invalid code: must be the code of the GitHub callback")
	case m.GetState() == "" || len(m.GetState()) > maxStateLength:
		return nil, api.ErrInvalidf("invalid state: must be the state of the GitHub callback")
	}
	// The same limit as the OwnSand link validation: every call reaches
	// GitHub.
	if ok, retry := s.Limits.PerUser.Allow("github-link:"+c.UserID, 1); !ok {
		return nil, api.ErrRateLimited(retry)
	}
	// The state is single-use: it is consumed before the GitHub calls, so a
	// failed link needs a new installation URL.
	tag, err := s.DB.Pool.Exec(ctx, "DELETE FROM github_install_states WHERE state_hash = $1 AND organization_id = $2 AND user_id = $3 AND expire_time > $4",
		hashState(m.GetState()), c.OrganizationID, c.UserID, s.NowUTC())
	if err != nil {
		return nil, s.Internal(ctx, "consume install state", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, api.ErrInvalidf("invalid state: unknown, expired, already used or created by another user or organization; start the installation again")
	}
	token, err := l.gh.ExchangeCode(ctx, m.GetCode())
	if errors.Is(err, githubapi.ErrBadCode) {
		return nil, api.ErrInvalidf("invalid code: GitHub rejected it; start the installation again")
	}
	if err != nil {
		return nil, l.gitHubUnavailable(ctx, "exchange oauth code", err)
	}
	list, err := l.gh.UserInstallations(ctx, token)
	if err != nil {
		return nil, l.gitHubUnavailable(ctx, "list user installations", err)
	}
	var found *githubapi.Installation
	for i := range list {
		if list[i].ID == ghID {
			found = &list[i]
		}
	}
	if found == nil || (found.AppID != 0 && found.AppID != l.gh.AppID()) {
		return nil, api.ErrPermission("the GitHub user cannot access this installation of the tide GitHub App")
	}
	if !found.IsOrganization() {
		return nil, api.ErrPrecondition("the installation is on a personal account; tide registers only organization runners, so install the app on a GitHub organization")
	}
	if found.SuspendedAt != nil {
		return nil, api.ErrPrecondition("the installation is suspended in GitHub; unsuspend it first")
	}
	groups, err := l.gh.RunnerGroups(ctx, ghID, found.Account.Login)
	if err != nil {
		return nil, l.gitHubUnavailable(ctx, "list runner groups", err)
	}
	var group *githubapi.RunnerGroup
	for i := range groups {
		if groups[i].Default || (group == nil && groups[i].Name == "Default") {
			group = &groups[i]
		}
	}
	if group == nil {
		return nil, api.ErrPrecondition("the GitHub organization has no Default runner group")
	}
	var out *Installation
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		existing, err := l.byGitHubID(ctx, tx, ghID, true)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.OrganizationID == c.OrganizationID {
				return api.ErrAlreadyExists("the installation is already linked to this organization")
			}
			return api.ErrAlreadyExists("the installation is linked to another tide organization")
		}
		now := s.NowUTC()
		out, err = scanInstallation(tx.QueryRow(ctx, `INSERT INTO github_installations (installation_id, organization_id, github_installation_id, github_account_login,
			github_account_id, state, github_runner_group_id, runner_group_name, linked_by_user_id, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) RETURNING `+installationColumns,
			api.NewID(), c.OrganizationID, ghID, found.Account.Login, found.Account.ID, int16(tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE),
			group.ID, group.Name, c.UserID, now))
		if db.IsUniqueViolation(err) {
			return api.ErrAlreadyExists("the installation is linked to another tide organization")
		}
		if err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_LINKED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_GITHUB_INSTALLATION, TargetID: out.ID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "link github installation", err)
	}
	s.Logger.InfoContext(ctx, "github_installation_linked", slog.String("organization_id", c.OrganizationID), slog.String("installation_id", out.ID),
		slog.Int64("github_installation_id", ghID), slog.Int64("github_runner_group_id", group.ID))
	s.WorkChanged(c.OrganizationID)
	return connect.NewResponse(&tidev1.LinkGitHubInstallationResponse{Installation: out.proto()}), nil
}

// ListGitHubInstallations lists the links of the organization. It reads
// only stored state, so it also works without a GitHub App configuration.
func (h *Handler) ListGitHubInstallations(ctx context.Context, req *connect.Request[tidev1.ListGitHubInstallationsRequest]) (*connect.Response[tidev1.ListGitHubInstallationsResponse], error) {
	s := h.l.svc
	m := req.Msg
	c, err := s.Authorize(ctx, s.DB.Pool, m.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+installationColumns+" FROM github_installations WHERE organization_id = $1 AND installation_id > $2 ORDER BY installation_id LIMIT $3",
		c.OrganizationID, api.AfterKey(after), limit)
	if err != nil {
		return nil, s.Internal(ctx, "list github installations", err)
	}
	defer rows.Close()
	resp := &tidev1.ListGitHubInstallationsResponse{}
	last := ""
	for rows.Next() {
		i, err := scanInstallation(rows)
		if err != nil {
			return nil, s.Internal(ctx, "list github installations", err)
		}
		resp.Installations = append(resp.Installations, i.proto())
		last = i.ID
	}
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "list github installations", err)
	}
	resp.NextPageToken = api.NextPageToken(len(resp.Installations), limit, last)
	return connect.NewResponse(resp), nil
}

// activeInstallation authorizes a call on one installation that needs
// GitHub and loads the installation, which must be ACTIVE.
func (l *Layer) activeInstallation(ctx context.Context, orgID, installationID string) (api.Caller, *Installation, error) {
	s := l.svc
	c, err := s.Authorize(ctx, s.DB.Pool, orgID, authz.ActionManageIntegrations)
	if err != nil {
		return c, nil, err
	}
	id, err := api.NormalizeID("installation_id", installationID)
	if err != nil {
		return c, nil, err
	}
	if !l.Configured() {
		return c, nil, errNotConfigured
	}
	inst, err := l.installation(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return c, nil, s.Internal(ctx, "get github installation", err)
	}
	if !inst.Active() {
		return c, nil, api.ErrPrecondition("the installation is not active in GitHub")
	}
	return c, inst, nil
}

func groupProto(g githubapi.RunnerGroup) *tidev1.GitHubRunnerGroup {
	return &tidev1.GitHubRunnerGroup{GithubRunnerGroupId: g.ID, Name: g.Name, Visibility: g.Visibility, IsDefault: g.Default}
}

// ListGitHubRunnerGroups lists the runner groups of the installation's
// organization. It calls GitHub, so it needs the role of the installation
// management (recorded decision).
func (h *Handler) ListGitHubRunnerGroups(ctx context.Context, req *connect.Request[tidev1.ListGitHubRunnerGroupsRequest]) (*connect.Response[tidev1.ListGitHubRunnerGroupsResponse], error) {
	l := h.l
	_, inst, err := l.activeInstallation(ctx, req.Msg.GetOrganizationId(), req.Msg.GetInstallationId())
	if err != nil {
		return nil, err
	}
	groups, err := l.gh.RunnerGroups(ctx, inst.GitHubID, inst.Login)
	if err != nil {
		return nil, l.gitHubUnavailable(ctx, "list runner groups", err)
	}
	resp := &tidev1.ListGitHubRunnerGroupsResponse{}
	for _, g := range groups {
		resp.RunnerGroups = append(resp.RunnerGroups, groupProto(g))
	}
	return connect.NewResponse(resp), nil
}

// UpdateGitHubInstallation sets the runner group of an installation after
// it checks with GitHub that the group exists. Runners that start after the
// change register in the new group.
func (h *Handler) UpdateGitHubInstallation(ctx context.Context, req *connect.Request[tidev1.UpdateGitHubInstallationRequest]) (*connect.Response[tidev1.UpdateGitHubInstallationResponse], error) {
	l := h.l
	s := l.svc
	c, inst, err := l.activeInstallation(ctx, req.Msg.GetOrganizationId(), req.Msg.GetInstallationId())
	if err != nil {
		return nil, err
	}
	groupID := req.Msg.GetGithubRunnerGroupId()
	if groupID <= 0 {
		return nil, api.ErrInvalidf("invalid github_runner_group_id: must be positive")
	}
	groups, err := l.gh.RunnerGroups(ctx, inst.GitHubID, inst.Login)
	if err != nil {
		return nil, l.gitHubUnavailable(ctx, "list runner groups", err)
	}
	var group *githubapi.RunnerGroup
	for i := range groups {
		if groups[i].ID == groupID {
			group = &groups[i]
		}
	}
	if group == nil {
		return nil, api.ErrInvalidf("invalid github_runner_group_id: the GitHub organization has no such runner group")
	}
	var out *Installation
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		out, err = scanInstallation(tx.QueryRow(ctx, `UPDATE github_installations SET github_runner_group_id = $3, runner_group_name = $4, update_time = $5
			WHERE organization_id = $1 AND installation_id = $2 RETURNING `+installationColumns, c.OrganizationID, inst.ID, group.ID, group.Name, s.NowUTC()))
		if errors.Is(err, pgx.ErrNoRows) {
			return api.ErrNotFound("GitHub installation")
		}
		if err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_UPDATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_GITHUB_INSTALLATION, TargetID: inst.ID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "update github installation", err)
	}
	s.Logger.InfoContext(ctx, "github_installation_updated", slog.String("organization_id", c.OrganizationID), slog.String("installation_id", inst.ID),
		slog.Int64("github_runner_group_id", group.ID))
	return connect.NewResponse(&tidev1.UpdateGitHubInstallationResponse{Installation: out.proto()}), nil
}

// UnlinkGitHubInstallation removes a link. It first removes the GitHub
// registrations of the installation's runners that wait for a job (while
// the app can still reach the installation), then, in one transaction,
// deletes the link, cancels the QUEUED requests, ends the started ones and
// deletes the waiting runners. A runner whose removal GitHub refused took a
// job and finishes it. It also works without a GitHub App configuration;
// there is then no registration to remove.
func (h *Handler) UnlinkGitHubInstallation(ctx context.Context, req *connect.Request[tidev1.UnlinkGitHubInstallationRequest]) (*connect.Response[tidev1.UnlinkGitHubInstallationResponse], error) {
	l := h.l
	s := l.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageIntegrations)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("installation_id", req.Msg.GetInstallationId())
	if err != nil {
		return nil, err
	}
	inst, err := l.installation(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, s.Internal(ctx, "get github installation", err)
	}
	skip := map[string]bool{}
	if l.Configured() && inst.Active() {
		idle, err := l.idleRunners(ctx, s.DB.Pool, c.OrganizationID, inst.PoolKey())
		if err != nil {
			return nil, s.Internal(ctx, "list runners", err)
		}
		for _, r := range idle {
			refused, err := l.removeRegistration(ctx, inst, r.ID)
			if err != nil {
				return nil, l.gitHubUnavailable(ctx, "remove runner registration", err)
			}
			skip[r.ID] = refused
		}
	}
	var deleted []string
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		cur, err := l.byGitHubID(ctx, tx, inst.GitHubID, true)
		if err != nil {
			return err
		}
		if cur == nil || cur.ID != inst.ID {
			return api.ErrNotFound("GitHub installation")
		}
		if deleted, err = l.deactivateTx(ctx, tx, cur, skip); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM github_installations WHERE installation_id = $1", inst.ID); err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_UNLINKED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_GITHUB_INSTALLATION, TargetID: inst.ID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "unlink github installation", err)
	}
	for _, r := range deleted {
		l.engine.DeleteSandboxOf(context.WithoutCancel(ctx), c.OrganizationID, r)
	}
	s.WorkChanged(c.OrganizationID)
	s.Logger.InfoContext(ctx, "github_installation_unlinked", slog.String("organization_id", c.OrganizationID), slog.String("installation_id", inst.ID),
		slog.Int64("github_installation_id", inst.GitHubID), slog.Int("runners_deleted", len(deleted)))
	return connect.NewResponse(&tidev1.UnlinkGitHubInstallationResponse{}), nil
}
