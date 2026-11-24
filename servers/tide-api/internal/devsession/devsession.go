// SPDX-License-Identifier: FSL-1.1-ALv2

// Package devsession implements DevSessionService and the DEV_SESSION
// adapter of the work engine. The owning contract is the Dev sessions, Dev
// session commands and Dev session file transfer sections of
// docs/servers-tide-api-foundation.md.
//
// Every RPC accepts a Logto access token or a personal API key. Commands
// and file transfers go to the OwnSand data path with the organization's
// OwnSand API key; only the session owner can run them. Exec environment
// values, command arguments, output and file contents are never stored,
// logged or audited.
package devsession

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// Limits of the contract.
const (
	maxLabelLength = 100
	// StreamLimit ends a forwarded exec or read stream before 14 minutes,
	// with an OK status and no final event.
	StreamLimit = 13*time.Minute + 30*time.Second
	// KeepaliveInterval is the longest time without an exec event; the
	// contract requires at least one keepalive every 60 seconds.
	KeepaliveInterval = 30 * time.Second
	// maxChunk is the largest file chunk.
	maxChunk = 1 << 20
)

// Timing overrides the stream durations (tests).
type Timing struct {
	StreamLimit       time.Duration
	KeepaliveInterval time.Duration
}

// Handler serves DevSessionService.
type Handler struct {
	svc    *api.Service
	e      *work.Engine
	timing Timing
	writes *writes
}

var _ tidev1connect.DevSessionServiceHandler = (*Handler)(nil)

// Register mounts DevSessionService, registers the DEV_SESSION adapter and
// the membership end hook.
func Register(r *app.Registry, e *work.Engine, timing Timing) *Handler {
	if timing.StreamLimit <= 0 {
		timing.StreamLimit = StreamLimit
	}
	if timing.KeepaliveInterval <= 0 {
		timing.KeepaliveInterval = KeepaliveInterval
	}
	h := &Handler{svc: r.Service, e: e, timing: timing, writes: newWrites()}
	e.RegisterAdapter(Adapter{})
	r.Handle(tidev1connect.NewDevSessionServiceHandler(h, r.HandlerOptions()...))
	r.OnMembershipEnd(api.MembershipEndHook{Name: "dev_sessions", InTx: h.membershipEndTx, AfterCommit: h.membershipEndAfter})
	return h
}

func newExecID() string {
	id, err := uuidv7.NewString()
	if err != nil {
		panic(err)
	}
	return id
}

// session is a dev session with its runner and profile details.
type session struct {
	req         *work.Request
	runner      *work.Runner
	profileName string
	guestOS     tidev1.GuestOs
	workspace   string
}

func (s *session) proto() *tidev1.DevSession {
	r := s.req
	out := &tidev1.DevSession{
		DevSessionId: r.ID, OrganizationId: r.OrganizationID, RunnerProfileId: r.ProfileID, OwnerUserId: r.OwnerUserID, Label: r.Label,
		GuestOs: s.guestOS, WorkspacePath: s.workspace, State: r.State, EndReason: r.EndReason, QueuedAt: api.Timestamp(r.QueueTime),
		StartedAt: api.TimestampPtr(r.StartTime), EndedAt: api.TimestampPtr(r.EndTime), RunnerId: r.RunnerID, RunnerProfileName: s.profileName,
	}
	if s.runner != nil {
		out.OwnsandDeviceId = s.runner.DeviceID
	}
	return out
}

// complete loads the runner and profile details of requests.
func (h *Handler) complete(ctx context.Context, q db.Querier, reqs []*work.Request) ([]*session, error) {
	out := make([]*session, len(reqs))
	var runnerIDs, profileIDs []string
	for i, r := range reqs {
		out[i] = &session{req: r}
		if r.RunnerID != "" {
			runnerIDs = append(runnerIDs, r.RunnerID)
		}
		profileIDs = append(profileIDs, r.ProfileID)
	}
	runners := map[string]*work.Runner{}
	if len(runnerIDs) > 0 {
		rows, err := q.Query(ctx, "SELECT "+work.RunnerColumns+" FROM runners WHERE runner_id = ANY($1::uuid[])", runnerIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := work.ScanRunner(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			runners[r.ID] = r
		}
		rows.Close()
	}
	type prof struct {
		name, workspace string
		guest           int16
	}
	profiles := map[string]prof{}
	rows, err := q.Query(ctx, "SELECT runner_profile_id::text, name, workspace_path, guest_os FROM runner_profiles WHERE runner_profile_id = ANY($1::uuid[])", profileIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p prof
		if err := rows.Scan(&id, &p.name, &p.workspace, &p.guest); err != nil {
			rows.Close()
			return nil, err
		}
		profiles[id] = p
	}
	rows.Close()
	for _, s := range out {
		if p, ok := profiles[s.req.ProfileID]; ok {
			s.profileName, s.workspace, s.guestOS = p.name, p.workspace, tidev1.GuestOs(p.guest)
		}
		if r := runners[s.req.RunnerID]; r != nil {
			s.runner = r
			// The runner keeps the settings that it started with.
			s.workspace, s.guestOS = r.WorkspacePath, r.GuestOS
		}
	}
	return out, nil
}

// load reads one dev session of the organization (not_found otherwise).
func (h *Handler) load(ctx context.Context, q db.Querier, orgID, rawID string) (*session, error) {
	id, err := api.NormalizeID("dev_session_id", rawID)
	if err != nil {
		return nil, err
	}
	r, err := work.ScanRequest(q.QueryRow(ctx, "SELECT "+work.RequestColumns+" FROM requests WHERE organization_id = $1 AND request_id = $2 AND kind = $3",
		orgID, id, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("dev session")
	}
	if err != nil {
		return nil, err
	}
	all, err := h.complete(ctx, q, []*work.Request{r})
	if err != nil {
		return nil, err
	}
	return all[0], nil
}

func (h *Handler) ListDevSessionProfiles(ctx context.Context, req *connect.Request[tidev1.ListDevSessionProfilesRequest]) (*connect.Response[tidev1.ListDevSessionProfilesResponse], error) {
	s := h.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	all, err := h.e.Profiles().ListAll(ctx, s.DB.Pool, c.OrganizationID)
	if err != nil {
		return nil, s.Internal(ctx, "list dev session profiles", err)
	}
	resp := &tidev1.ListDevSessionProfilesResponse{}
	last := ""
	for _, p := range all {
		if p.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION || p.ID <= after {
			continue
		}
		if len(resp.Profiles) == int(limit) {
			break
		}
		pp := p.Proto()
		resp.Profiles = append(resp.Profiles, &tidev1.DevSessionProfile{
			RunnerProfileId: p.ID, Name: p.Name, GuestOs: p.GuestOS, Resources: pp.GetResources(), WorkspacePath: p.WorkspacePath,
			IdleTtl: pp.GetIdleTtl(), MaxTtl: pp.GetMaxTtl(), MaxQueueWait: pp.GetMaxQueueWait(),
		})
		last = p.ID
	}
	resp.NextPageToken = api.NextPageToken(len(resp.Profiles), limit, last)
	return connect.NewResponse(resp), nil
}

func validateLabel(label string) error {
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > maxLabelLength {
		return api.ErrInvalidf("invalid label: must be at most %d characters", maxLabelLength)
	}
	for _, r := range label {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return api.ErrInvalidf("invalid label: must not contain control characters")
		}
	}
	return nil
}

// CreateDevSession creates a QUEUED dev session request and returns at
// once. The per-user limit counts the caller's sessions in QUEUED, STARTING
// and RUNNING; the membership row lock serializes the caller's creations.
func (h *Handler) CreateDevSession(ctx context.Context, req *connect.Request[tidev1.CreateDevSessionRequest]) (*connect.Response[tidev1.CreateDevSessionResponse], error) {
	s := h.svc
	var out *session
	var orgID string
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions)
		if err != nil {
			return err
		}
		orgID = c.OrganizationID
		profileID, err := api.NormalizeID("runner_profile_id", req.Msg.GetRunnerProfileId())
		if err != nil {
			return err
		}
		if err := validateLabel(req.Msg.GetLabel()); err != nil {
			return err
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		// A shared lock of the profile row until the commit: a concurrent
		// DeleteRunnerProfile, which locks the row FOR UPDATE before its
		// hook cancels the queued requests, either waits and cancels this
		// request too, or deletes the row first and this call fails with
		// not_found.
		var locked int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM runner_profiles WHERE organization_id = $1 AND runner_profile_id = $2 FOR SHARE",
			c.OrganizationID, profileID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return api.ErrNotFound("runner profile")
			}
			return err
		}
		p, err := h.e.Profiles().Get(ctx, tx, c.OrganizationID, profileID)
		if err != nil {
			return err
		}
		if p.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION {
			return api.ErrInvalidf("invalid runner_profile_id: the profile is not a DEV_SESSION profile")
		}
		var limit *int32
		if err := tx.QueryRow(ctx, "SELECT 1 FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", c.OrganizationID, c.UserID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return api.ErrNotFound("organization")
			}
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT max_dev_sessions_per_user FROM organizations WHERE organization_id = $1", c.OrganizationID).Scan(&limit); err != nil {
			return err
		}
		if limit != nil {
			var open int32
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM requests WHERE organization_id = $1 AND kind = $2 AND owner_user_id = $3 AND state = ANY($4)",
				c.OrganizationID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), c.UserID, work.OpenRequestStates()).Scan(&open); err != nil {
				return err
			}
			if open >= *limit {
				return connect.NewError(connect.CodeResourceExhausted, errors.New("the per-user dev session limit of the organization is reached"))
			}
		}
		r, _, err := h.e.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: work.SourceDevSession, OwnerUserID: c.UserID, Label: req.Msg.GetLabel()})
		if err != nil {
			return err
		}
		if err := s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_DEV_SESSION_CREATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_DEV_SESSION, TargetID: r.ID, SubjectUserID: c.UserID,
		}); err != nil {
			return err
		}
		out = &session{req: r, profileName: p.Name, guestOS: p.GuestOS, workspace: p.WorkspacePath}
		return nil
	})
	if err != nil {
		return nil, s.Internal(ctx, "create dev session", err)
	}
	s.WorkChanged(orgID)
	return connect.NewResponse(&tidev1.CreateDevSessionResponse{DevSession: out.proto()}), nil
}

func (h *Handler) GetDevSession(ctx context.Context, req *connect.Request[tidev1.GetDevSessionRequest]) (*connect.Response[tidev1.GetDevSessionResponse], error) {
	s := h.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions)
	if err != nil {
		return nil, err
	}
	sess, err := h.load(ctx, s.DB.Pool, c.OrganizationID, req.Msg.GetDevSessionId())
	if err != nil {
		return nil, s.Internal(ctx, "get dev session", err)
	}
	return connect.NewResponse(&tidev1.GetDevSessionResponse{DevSession: sess.proto()}), nil
}

func (h *Handler) ListDevSessions(ctx context.Context, req *connect.Request[tidev1.ListDevSessionsRequest]) (*connect.Response[tidev1.ListDevSessionsResponse], error) {
	s := h.svc
	m := req.Msg
	c, err := s.Authorize(ctx, s.DB.Pool, m.GetOrganizationId(), authz.ActionUseDevSessions)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	owner := ""
	switch o := m.GetOwner().(type) {
	case *tidev1.ListDevSessionsRequest_OwnerUserId:
		if o.OwnerUserId == "" {
			return nil, api.ErrInvalidf("invalid owner_user_id: is empty")
		}
		owner = o.OwnerUserId
	case *tidev1.ListDevSessionsRequest_OwnedByCaller:
		if o.OwnedByCaller {
			owner = c.UserID
		}
	}
	profileID := ""
	if m.GetRunnerProfileId() != "" {
		if profileID, err = api.NormalizeID("runner_profile_id", m.GetRunnerProfileId()); err != nil {
			return nil, err
		}
	}
	states, err := work.RequestStateFilter(m.GetStates(), m.GetIncludeEnded())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+work.RequestColumns+` FROM requests
		WHERE organization_id = $1 AND kind = $2 AND request_id > $3 AND ($4 = '' OR owner_user_id = $4) AND ($5 = '' OR runner_profile_id::text = $5)
		AND ($6::smallint[] IS NULL OR state = ANY($6)) ORDER BY request_id LIMIT $7`,
		c.OrganizationID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), api.AfterKey(after), owner, profileID, states, limit)
	if err != nil {
		return nil, s.Internal(ctx, "list dev sessions", err)
	}
	var reqs []*work.Request
	for rows.Next() {
		r, err := work.ScanRequest(rows)
		if err != nil {
			rows.Close()
			return nil, s.Internal(ctx, "list dev sessions", err)
		}
		reqs = append(reqs, r)
	}
	rows.Close()
	all, err := h.complete(ctx, s.DB.Pool, reqs)
	if err != nil {
		return nil, s.Internal(ctx, "list dev sessions", err)
	}
	resp := &tidev1.ListDevSessionsResponse{}
	last := ""
	for _, sess := range all {
		resp.DevSessions = append(resp.DevSessions, sess.proto())
		last = sess.req.ID
	}
	resp.NextPageToken = api.NextPageToken(len(all), limit, last)
	return connect.NewResponse(resp), nil
}

// errRebound restarts a DeleteDevSession transaction whose session got a
// runner after the runner lock.
var errRebound = errors.New("the dev session got a runner")

// deleteAttempts bounds the restarts of DeleteDevSession after errRebound.
const deleteAttempts = 3

// DeleteDevSession cancels a queued session (CANCELED_BY_USER) or deletes a
// starting or running one (ENDED, SESSION_DELETED) with its sandbox. The
// session owner, an owner or an admin may delete it.
func (h *Handler) DeleteDevSession(ctx context.Context, req *connect.Request[tidev1.DeleteDevSessionRequest]) (*connect.Response[tidev1.DeleteDevSessionResponse], error) {
	s := h.svc
	var d *work.Deletion
	var orgID, sessionID string
	var err error
	for attempt := 1; ; attempt++ {
		d = nil
		err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
			c, err := s.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionDeleteDevSession)
			if err != nil {
				return err
			}
			orgID = c.OrganizationID
			if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
				return err
			}
			sess, err := h.load(ctx, tx, c.OrganizationID, req.Msg.GetDevSessionId())
			if err != nil {
				return err
			}
			sessionID = sess.req.ID
			if !authz.CanDeleteDevSession(c.Role, c.UserID, sess.req.OwnerUserID) {
				return api.ErrPermission("a member can delete only their own dev sessions")
			}
			// Decide on the locked state, so a dispatch that starts the
			// session meanwhile is seen: lock the open runner of the session,
			// then the request, in the engine's lock order (runner first).
			var runnerID string
			err = tx.QueryRow(ctx, "SELECT runner_id::text FROM runners WHERE request_id = $1 AND state <> ALL($2) FOR UPDATE",
				sessionID, []int16{int16(tidev1.RunnerState_RUNNER_STATE_DELETED), int16(tidev1.RunnerState_RUNNER_STATE_FAILED)}).Scan(&runnerID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			cur, err := work.ScanRequest(tx.QueryRow(ctx, "SELECT "+work.RequestColumns+" FROM requests WHERE request_id = $1 FOR UPDATE", sessionID))
			if err != nil {
				return err
			}
			if work.RequestTerminal(cur.State) {
				return nil
			}
			queued := cur.State == tidev1.RequestState_REQUEST_STATE_QUEUED
			ended := false
			if !queued && cur.RunnerID != "" {
				if cur.RunnerID != runnerID {
					r, err := h.e.GetRunner(ctx, tx, c.OrganizationID, cur.RunnerID)
					if err != nil {
						return err
					}
					if !work.RunnerTerminal(r.State) {
						// The runner was bound after the runner lock; locking
						// it now would invert the lock order.
						return errRebound
					}
				}
				d, err = h.e.DeleteRunnerTx(ctx, tx, c.OrganizationID, cur.RunnerID, tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER, work.DeleteByUser)
				if err != nil {
					return err
				}
				after, err := h.e.GetRequest(ctx, tx, c.OrganizationID, sessionID)
				if err != nil {
					return err
				}
				ended = work.RequestTerminal(after.State)
			}
			if !ended {
				end := work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER}
				if !queued {
					end = work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED}
				}
				if _, err := h.e.EndRequestTx(ctx, tx, sessionID, end); err != nil {
					return err
				}
			}
			return s.WriteAudit(ctx, tx, api.AuditEvent{
				OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_DEV_SESSION_DELETED, Actor: c.Principal,
				TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_DEV_SESSION, TargetID: sessionID, SubjectUserID: sess.req.OwnerUserID,
			})
		})
		if !errors.Is(err, errRebound) {
			break
		}
		if attempt == deleteAttempts {
			err = api.ErrUnavailable("the dev session is starting; try again")
			break
		}
	}
	if err != nil {
		return nil, s.Internal(ctx, "delete dev session", err)
	}
	h.e.FinishDeletion(ctx, d)
	h.e.Kick(orgID)
	sess, err := h.load(ctx, s.DB.Pool, orgID, sessionID)
	if err != nil {
		return nil, s.Internal(ctx, "delete dev session", err)
	}
	return connect.NewResponse(&tidev1.DeleteDevSessionResponse{DevSession: sess.proto()}), nil
}

// membershipEndTx ends the removed member's dev sessions of the
// organization: queued ones with CANCELED and MEMBER_REMOVED, the others
// with ENDED and MEMBER_REMOVED; their runners are DELETING, and
// membershipEndAfter deletes the sandboxes (the lease holder's drivers
// retry).
func (h *Handler) membershipEndTx(ctx context.Context, tx pgx.Tx, m api.MembershipEnd) error {
	rows, err := tx.Query(ctx, "SELECT request_id::text, state, COALESCE(runner_id::text, '') FROM requests WHERE organization_id = $1 AND kind = $2 AND owner_user_id = $3 AND state = ANY($4)",
		m.OrganizationID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), m.UserID, work.OpenRequestStates())
	if err != nil {
		return err
	}
	type open struct {
		id, runner string
		state      int16
	}
	var list []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.state, &o.runner); err != nil {
			rows.Close()
			return err
		}
		list = append(list, o)
	}
	rows.Close()
	for _, o := range list {
		if o.runner != "" && tidev1.RequestState(o.state) != tidev1.RequestState_REQUEST_STATE_QUEUED {
			if _, err := h.e.DeleteRunnerTx(ctx, tx, m.OrganizationID, o.runner, tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER, work.DeleteMemberRemoved); err != nil {
				return err
			}
		}
		if _, err := h.e.EndRequestTx(ctx, tx, o.id, work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED}); err != nil {
			return err
		}
	}
	return nil
}

// membershipEndAfter deletes the sandboxes of the runners that
// membershipEndTx marked.
func (h *Handler) membershipEndAfter(ctx context.Context, m api.MembershipEnd) {
	rows, err := h.svc.DB.Pool.Query(ctx, `SELECT n.runner_id::text FROM runners n JOIN requests r ON r.request_id = n.request_id
		WHERE n.organization_id = $1 AND n.state = $2 AND r.owner_user_id = $3 AND r.end_reason = $4`,
		m.OrganizationID, int16(tidev1.RunnerState_RUNNER_STATE_DELETING), m.UserID, int16(tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED))
	if err != nil {
		h.svc.Logger.WarnContext(ctx, "dev_session_member_cleanup_failed", slog.String("organization_id", m.OrganizationID), slog.Any("error", err))
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		h.e.DeleteSandboxOf(ctx, m.OrganizationID, id)
	}
	h.e.Kick(m.OrganizationID)
}

// resolvePath returns an absolute guest path: an absolute path of the guest
// OS unchanged, or a path relative to the workspace path joined to it with
// the guest's separator. An empty path is the workspace path.
func resolvePath(guest tidev1.GuestOs, workspace, path string) (string, error) {
	if strings.IndexByte(path, 0) >= 0 || !utf8.ValidString(path) {
		return "", api.ErrInvalidf("invalid path: must be valid UTF-8 without NUL")
	}
	if path == "" {
		return workspace, nil
	}
	if guest == tidev1.GuestOs_GUEST_OS_WINDOWS {
		if len(path) >= 3 && isLetter(path[0]) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') || strings.HasPrefix(path, `\\`) {
			return path, nil
		}
		return strings.TrimRight(workspace, `\`) + `\` + strings.ReplaceAll(path, "/", `\`), nil
	}
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	return strings.TrimRight(workspace, "/") + "/" + path, nil
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// operable loads a dev session for an exec or a file transfer: the caller
// must be its owner and it must be RUNNING.
func (h *Handler) operable(ctx context.Context, orgIDRaw, sessionIDRaw string) (*session, api.Caller, error) {
	s := h.svc
	c, err := s.Authorize(ctx, s.DB.Pool, orgIDRaw, authz.ActionOperateDevSession)
	if err != nil {
		return nil, c, err
	}
	sess, err := h.load(ctx, s.DB.Pool, c.OrganizationID, sessionIDRaw)
	if err != nil {
		return nil, c, s.Internal(ctx, "load dev session", err)
	}
	if !authz.CanOperateDevSession(c.UserID, sess.req.OwnerUserID) {
		return nil, c, api.ErrPermission("only the session owner can run commands and transfer files")
	}
	if sess.req.State != tidev1.RequestState_REQUEST_STATE_RUNNING || sess.runner == nil || sess.runner.SandboxID == "" {
		return nil, c, api.ErrPrecondition("the dev session is not RUNNING")
	}
	return sess, c, nil
}

// ownsandError maps an OwnSand error of a data path operation to the tide
// error. Messages of OwnSand errors never contain secrets.
func ownsandError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeAlreadyExists,
			connect.CodeResourceExhausted, connect.CodeAborted, connect.CodeOutOfRange, connect.CodeUnavailable, connect.CodeDeadlineExceeded:
			return connect.NewError(ce.Code(), errors.New("OwnSand: "+ce.Message()))
		case connect.CodeUnauthenticated, connect.CodePermissionDenied:
			return api.ErrPrecondition("OwnSand rejected the organization's OwnSand API key")
		}
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	var validationErr *validate.Error
	if errors.As(err, &validationErr) {
		return api.ErrInvalidf("%s", validationErr.Error())
	}
	return api.ErrUnavailable("the OwnSand data path failed; try again")
}
