// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
)

// Stream limits of WatchQueue (protos contract).
const (
	watchKeepalive = 25 * time.Second
	watchLifetime  = 13*time.Minute + 30*time.Second
	watchPoll      = time.Second
)

// Handler serves WorkService.
type Handler struct {
	e *Engine
	// watch overrides the WatchQueue durations (tests).
	watchKeepalive, watchLifetime, watchPoll time.Duration
}

var _ tidev1connect.WorkServiceHandler = (*Handler)(nil)

// offsetToken is the opaque page token of the lists whose order is not a
// UUID order: `o1:` and the number of rows before the page, in unpadded
// base64url.
func offsetToken(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o1:" + strconv.Itoa(n)))
}

func parseOffsetToken(size int32, token string) (int32, int, error) {
	limit, _, err := api.PageArgs(size, "")
	if err != nil || token == "" {
		return limit, 0, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), "o1:") {
		return 0, 0, api.ErrInvalidf("invalid page_token")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "o1:"))
	if err != nil || n < 0 {
		return 0, 0, api.ErrInvalidf("invalid page_token")
	}
	return limit, n, nil
}

// RequestStateFilter returns the SQL state filter for requests and dev sessions.
func RequestStateFilter(states []tidev1.RequestState, includeEnded bool) ([]int16, error) {
	return stateList(states, "RequestState", tidev1.RequestState_name, openRequestStates, includeEnded)
}

func stateList[T ~int32](states []T, enum string, names map[int32]string, open []int16, includeEnded bool) ([]int16, error) {
	if len(states) > 0 {
		out := make([]int16, len(states))
		for i, s := range states {
			if err := api.DefinedEnum("states", enum, s, names); err != nil {
				return nil, err
			}
			out[i] = int16(s)
		}
		return out, nil
	}
	if includeEnded {
		return nil, nil
	}
	return open, nil
}

// ListRequests lists requests: the open ones in queue order first, then the
// ended ones, newest first.
func (h *Handler) ListRequests(ctx context.Context, req *connect.Request[tidev1.ListRequestsRequest]) (*connect.Response[tidev1.ListRequestsResponse], error) {
	s := h.e.svc
	m := req.Msg
	c, err := s.Authorize(ctx, s.DB.Pool, m.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, offset, err := parseOffsetToken(m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	profileID := ""
	if m.GetRunnerProfileId() != "" {
		if profileID, err = api.NormalizeID("runner_profile_id", m.GetRunnerProfileId()); err != nil {
			return nil, err
		}
	}
	kind := m.GetKind()
	if err := api.DefinedEnum("kind", "WorkloadKind", kind, tidev1.WorkloadKind_name); err != nil {
		return nil, err
	}
	states, err := RequestStateFilter(m.GetStates(), m.GetIncludeEnded())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+requestColumns+` FROM requests
		WHERE organization_id = $1 AND ($2 = '' OR runner_profile_id::text = $2) AND ($3 = 0 OR kind = $3) AND ($4::smallint[] IS NULL OR state = ANY($4))
		ORDER BY (state IN (4, 5, 6, 7)),
			CASE WHEN state IN (1, 2, 3) THEN priority END, CASE WHEN state IN (1, 2, 3) THEN queue_time END,
			CASE WHEN state IN (4, 5, 6, 7) THEN end_time END DESC, request_id
		LIMIT $5 OFFSET $6`, c.OrganizationID, profileID, int16(kind), states, limit, offset)
	if err != nil {
		return nil, s.Internal(ctx, "list requests", err)
	}
	defer rows.Close()
	resp := &tidev1.ListRequestsResponse{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, s.Internal(ctx, "list requests", err)
		}
		resp.Requests = append(resp.Requests, r.Proto())
	}
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "list requests", err)
	}
	h.e.decorate(ctx, c.OrganizationID, resp.Requests...)
	if len(resp.Requests) == int(limit) {
		resp.NextPageToken = offsetToken(offset + int(limit))
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) GetRequest(ctx context.Context, req *connect.Request[tidev1.GetRequestRequest]) (*connect.Response[tidev1.GetRequestResponse], error) {
	s := h.e.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("request_id", req.Msg.GetRequestId())
	if err != nil {
		return nil, err
	}
	r, err := h.e.GetRequest(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, s.Internal(ctx, "get request", err)
	}
	out := r.Proto()
	h.e.decorate(ctx, c.OrganizationID, out)
	return connect.NewResponse(&tidev1.GetRequestResponse{Request: out}), nil
}

// CancelRequest ends a QUEUED request with CANCELED and CANCELED_BY_USER.
func (h *Handler) CancelRequest(ctx context.Context, req *connect.Request[tidev1.CancelRequestRequest]) (*connect.Response[tidev1.CancelRequestResponse], error) {
	e := h.e
	s := e.svc
	var out *Request
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionCancelRequest)
		if err != nil {
			return err
		}
		id, err := api.NormalizeID("request_id", req.Msg.GetRequestId())
		if err != nil {
			return err
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		if _, err := e.GetRequest(ctx, tx, c.OrganizationID, id); err != nil {
			return err
		}
		// Check the state under the row lock that the end takes, so a
		// dispatch or another cancellation that changes the request
		// meanwhile is seen.
		cur, err := e.lockRequest(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.State != reqQueued {
			return api.ErrPrecondition("only a QUEUED request can be canceled")
		}
		out, _, err = e.endRequestTx(ctx, tx, id, RequestEnd{State: reqCanceled, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER})
		if err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_REQUEST, TargetID: id, SubjectUserID: cur.OwnerUserID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "cancel request", err)
	}
	e.releaseClaim(ctx, out)
	msg := out.Proto()
	e.decorate(ctx, out.OrganizationID, msg)
	return connect.NewResponse(&tidev1.CancelRequestResponse{Request: msg}), nil
}

func (h *Handler) ListRunners(ctx context.Context, req *connect.Request[tidev1.ListRunnersRequest]) (*connect.Response[tidev1.ListRunnersResponse], error) {
	s := h.e.svc
	m := req.Msg
	c, err := s.Authorize(ctx, s.DB.Pool, m.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	profileID := ""
	if m.GetRunnerProfileId() != "" {
		if profileID, err = api.NormalizeID("runner_profile_id", m.GetRunnerProfileId()); err != nil {
			return nil, err
		}
	}
	states, err := stateList(m.GetStates(), "RunnerState", tidev1.RunnerState_name, openRunnerStates, m.GetIncludeEnded())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+runnerColumns+` FROM runners
		WHERE organization_id = $1 AND runner_id > $2 AND ($3 = '' OR runner_profile_id::text = $3) AND ($4::smallint[] IS NULL OR state = ANY($4))
		ORDER BY runner_id LIMIT $5`, c.OrganizationID, api.AfterKey(after), profileID, states, limit)
	if err != nil {
		return nil, s.Internal(ctx, "list runners", err)
	}
	defer rows.Close()
	resp := &tidev1.ListRunnersResponse{}
	last := ""
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, s.Internal(ctx, "list runners", err)
		}
		resp.Runners = append(resp.Runners, r.Proto())
		last = r.ID
	}
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "list runners", err)
	}
	resp.NextPageToken = api.NextPageToken(len(resp.Runners), limit, last)
	return connect.NewResponse(resp), nil
}

func (h *Handler) GetRunner(ctx context.Context, req *connect.Request[tidev1.GetRunnerRequest]) (*connect.Response[tidev1.GetRunnerResponse], error) {
	s := h.e.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("runner_id", req.Msg.GetRunnerId())
	if err != nil {
		return nil, err
	}
	r, err := h.e.GetRunner(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, s.Internal(ctx, "get runner", err)
	}
	return connect.NewResponse(&tidev1.GetRunnerResponse{Runner: r.Proto()}), nil
}

// DeleteRunner deletes a runner and its sandbox; the work in it ends.
func (h *Handler) DeleteRunner(ctx context.Context, req *connect.Request[tidev1.DeleteRunnerRequest]) (*connect.Response[tidev1.DeleteRunnerResponse], error) {
	e := h.e
	s := e.svc
	var d *Deletion
	var cur *Runner
	var orgID string
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := s.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionDeleteRunner)
		if err != nil {
			return err
		}
		orgID = c.OrganizationID
		id, err := api.NormalizeID("runner_id", req.Msg.GetRunnerId())
		if err != nil {
			return err
		}
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		if cur, err = e.GetRunner(ctx, tx, c.OrganizationID, id); err != nil {
			return err
		}
		d, err = e.DeleteRunnerTx(ctx, tx, c.OrganizationID, id, tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER, DeleteByUser)
		if err != nil || d == nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_RUNNER_DELETED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_RUNNER, TargetID: id,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "delete runner", err)
	}
	r := e.FinishDeletion(ctx, d)
	if r == nil {
		r, err = e.GetRunner(ctx, s.DB.Pool, orgID, cur.ID)
		if err != nil {
			return nil, s.Internal(ctx, "delete runner", err)
		}
	}
	return connect.NewResponse(&tidev1.DeleteRunnerResponse{Runner: r.Proto()}), nil
}

func deviceState(s ownsandv1.DeviceState) tidev1.DeviceState {
	switch s {
	case ownsandv1.DeviceState_DEVICE_STATE_ONLINE:
		return tidev1.DeviceState_DEVICE_STATE_ONLINE
	case ownsandv1.DeviceState_DEVICE_STATE_OFFLINE:
		return tidev1.DeviceState_DEVICE_STATE_OFFLINE
	}
	return tidev1.DeviceState_DEVICE_STATE_UNSPECIFIED
}

// GetCapacity returns the OwnSand devices of the link with the tide runners
// on each device by state.
func (h *Handler) GetCapacity(ctx context.Context, req *connect.Request[tidev1.GetCapacityRequest]) (*connect.Response[tidev1.GetCapacityResponse], error) {
	e := h.e
	s := e.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	client, _, err := e.links.Client(ctx, c.OrganizationID)
	if err != nil {
		return nil, s.Internal(ctx, "get capacity", err)
	}
	devices, err := client.ListDevices(ctx)
	if err != nil {
		if isCode(err, connect.CodeUnavailable) || isCode(err, connect.CodeDeadlineExceeded) {
			return nil, api.ErrUnavailable("OwnSand is unavailable; try again")
		}
		if isCode(err, connect.CodeUnauthenticated) {
			return nil, api.ErrPrecondition("OwnSand rejected the API key; the link is now INVALID")
		}
		return nil, s.Internal(ctx, "get capacity", err)
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT ownsand_device_id, state, count(*) FROM runners WHERE organization_id = $1 AND state = ANY($2) AND ownsand_device_id <> ''
		GROUP BY ownsand_device_id, state ORDER BY ownsand_device_id, state`, c.OrganizationID, openRunnerStates)
	if err != nil {
		return nil, s.Internal(ctx, "get capacity", err)
	}
	counts := map[string][]*tidev1.RunnerStateCount{}
	for rows.Next() {
		var device string
		var state int16
		var n int32
		if err := rows.Scan(&device, &state, &n); err != nil {
			rows.Close()
			return nil, s.Internal(ctx, "get capacity", err)
		}
		counts[device] = append(counts[device], &tidev1.RunnerStateCount{State: tidev1.RunnerState(state), Count: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "get capacity", err)
	}
	resp := &tidev1.GetCapacityResponse{}
	for _, d := range devices {
		l, a := d.GetLimits(), d.GetAllocation()
		allocatedVCPU := int64(a.GetVcpuCount())
		if a != nil && a.VcpuTotalCount != nil {
			allocatedVCPU = a.GetVcpuTotalCount()
		}
		resp.Devices = append(resp.Devices, &tidev1.DeviceCapacity{
			OwnsandDeviceId: d.GetDeviceId(), Name: d.GetName(), State: deviceState(d.GetState()),
			HostOs: label(d.GetHostOs(), "HOST_OS_"), Arch: label(d.GetArch(), "ARCH_"),
			Limits:             &tidev1.DeviceLimits{VcpuCount: l.GetVcpuCount(), MemoryMib: l.GetMemoryMib(), DiskGib: l.GetDiskGib(), VcpuOvercommitPercent: l.GetVcpuOvercommitPercent()},
			EffectiveVcpuLimit: d.GetEffectiveVcpuLimitCount(),
			Allocation:         &tidev1.DeviceAllocation{VcpuCount: allocatedVCPU, MemoryMib: a.GetMemoryMib(), DiskGib: a.GetDiskGib(), MacosGuestCount: a.GetMacosGuestCount()},
			Runners:            counts[d.GetDeviceId()],
		})
	}
	return connect.NewResponse(resp), nil
}

// WatchQueue streams every open request and runner, then
// snapshot_complete, then each change (also the change to a terminal
// state). Changes are read by change_seq, which a trigger assigns on every
// write, so the stream works on every instance. A sequence value can commit
// after a higher one; the stream therefore reads a window behind its cursor
// again and sends each row version once.
func (h *Handler) WatchQueue(ctx context.Context, req *connect.Request[tidev1.WatchQueueRequest], stream *connect.ServerStream[tidev1.WatchQueueResponse]) error {
	e := h.e
	s := e.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return err
	}
	keepalive, lifetime, poll := watchKeepalive, watchLifetime, watchPoll
	if h.watchKeepalive > 0 {
		keepalive, lifetime, poll = h.watchKeepalive, h.watchLifetime, h.watchPoll
	}
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	var mu sync.Mutex
	send := func(m *tidev1.WatchQueueResponse) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(m)
	}
	sent := map[string]int64{}
	// removable holds the sent runners that the dispatcher may still remove
	// without a terminal state: a CREATING runner without a sandbox, whose
	// CreateSandbox OwnSand rejected for capacity (dropRunner).
	removable := map[string]*tidev1.Runner{}
	var cursor, removed int64
	emit := func(rows []watchRow) error {
		for _, r := range rows {
			if sent[r.id] >= r.seq {
				continue
			}
			sent[r.id] = r.seq
			cursor = max(cursor, r.seq)
			if run := r.msg.GetRunner(); run != nil {
				if run.GetState() == runCreating && run.GetOwnsandSandboxId() == "" {
					removable[run.GetRunnerId()] = run
				} else {
					delete(removable, run.GetRunnerId())
				}
			}
			if err := send(r.msg); err != nil {
				return err
			}
		}
		return nil
	}
	// emitRemoved sends a sent runner whose row is gone once more as
	// DELETED with CLEAN_UP, so a client drops it: the stream has no
	// removal event.
	emitRemoved := func() error {
		if len(removable) == 0 {
			return nil
		}
		ids := make([]string, 0, len(removable))
		for id := range removable {
			ids = append(ids, id)
		}
		rows, err := s.DB.Pool.Query(ctx, "SELECT runner_id::text FROM runners WHERE runner_id = ANY($1::uuid[])", ids)
		if err != nil {
			return err
		}
		exists := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			exists[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if exists[id] {
				continue
			}
			gone := proto.Clone(removable[id]).(*tidev1.Runner)
			gone.State, gone.EndReason, gone.EndedAt = runDeleted, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP, api.Timestamp(s.NowUTC())
			delete(removable, id)
			removed++
			if err := send(&tidev1.WatchQueueResponse{Event: &tidev1.WatchQueueResponse_Runner{Runner: gone}}); err != nil {
				return err
			}
		}
		return nil
	}
	snapshot, err := h.watchRows(ctx, c.OrganizationID, 0, true)
	if err != nil {
		return s.Internal(ctx, "watch queue", err)
	}
	if err := emit(snapshot); err != nil {
		return err
	}
	if cursor == 0 {
		if err := s.DB.Pool.QueryRow(ctx, "SELECT COALESCE(GREATEST((SELECT max(change_seq) FROM requests WHERE organization_id = $1), (SELECT max(change_seq) FROM runners WHERE organization_id = $1)), 0)",
			c.OrganizationID).Scan(&cursor); err != nil {
			return s.Internal(ctx, "watch queue", err)
		}
	}
	if err := send(&tidev1.WatchQueueResponse{Event: &tidev1.WatchQueueResponse_SnapshotComplete{SnapshotComplete: &tidev1.WatchQueueSnapshotComplete{}}}); err != nil {
		return err
	}
	const window = 1000
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	lastSend := time.Now()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// The stream ends before 14 minutes with an OK status; the
				// client reconnects.
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}
		rows, err := h.watchRows(ctx, c.OrganizationID, max(cursor-window, 0), false)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return s.Internal(ctx, "watch queue", err)
		}
		before, beforeRemoved := len(sent), removed
		prevCursor := cursor
		if err := emit(rows); err != nil {
			return err
		}
		if err := emitRemoved(); err != nil {
			if ctx.Err() != nil {
				continue
			}
			return s.Internal(ctx, "watch queue", err)
		}
		if cursor != prevCursor || len(sent) != before || removed != beforeRemoved {
			lastSend = time.Now()
		}
		for id, seq := range sent {
			if seq < cursor-window {
				delete(sent, id)
			}
		}
		if time.Since(lastSend) >= keepalive {
			if err := send(&tidev1.WatchQueueResponse{Event: &tidev1.WatchQueueResponse_Keepalive{Keepalive: &tidev1.WatchQueueKeepalive{}}}); err != nil {
				return err
			}
			lastSend = time.Now()
		}
	}
}

type watchRow struct {
	id  string
	seq int64
	msg *tidev1.WatchQueueResponse
}

// watchRows reads the open rows (snapshot) or the rows changed after seq,
// in change order.
func (h *Handler) watchRows(ctx context.Context, orgID string, after int64, snapshot bool) ([]watchRow, error) {
	s := h.e.svc
	cond := "change_seq > $2"
	args := []any{orgID, after}
	if snapshot {
		cond = "state = ANY($2)"
	}
	reqArgs := args
	if snapshot {
		reqArgs = []any{orgID, openRequestStates}
	}
	var out []watchRow
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+requestColumns+" FROM requests WHERE organization_id = $1 AND "+cond+" ORDER BY change_seq", reqArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, watchRow{id: "q" + r.ID, seq: r.ChangeSeq, msg: &tidev1.WatchQueueResponse{Event: &tidev1.WatchQueueResponse_Request{Request: r.Proto()}}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var reqs []*tidev1.Request
	for _, r := range out {
		reqs = append(reqs, r.msg.GetRequest())
	}
	h.e.decorate(ctx, orgID, reqs...)
	runArgs := args
	if snapshot {
		runArgs = []any{orgID, openRunnerStates}
	}
	rows, err = s.DB.Pool.Query(ctx, "SELECT "+runnerColumns+" FROM runners WHERE organization_id = $1 AND "+cond+" ORDER BY change_seq", runArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, watchRow{id: "r" + r.ID, seq: r.ChangeSeq, msg: &tidev1.WatchQueueResponse{Event: &tidev1.WatchQueueResponse_Runner{Runner: r.Proto()}}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortRows(out)
	return out, nil
}

func sortRows(rows []watchRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].seq < rows[j-1].seq; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
