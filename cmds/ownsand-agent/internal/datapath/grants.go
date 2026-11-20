package datapath

import (
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	// MaxGrants is the per-device relay grant limit.
	MaxGrants = 256
	// AdmissionTolerance is added to a grant's expiry for admission only.
	AdmissionTolerance = 30 * time.Second
	// GrantIdleRelease releases a grant without an open stream.
	GrantIdleRelease = 60 * time.Second
)

// streamEnder ends a relay stream with a session-end reason.
type streamEnder interface {
	endWith(reason ownsandv1.SessionEndReason)
}

// grant is one relay grant. Its expiry and stream deadline are measured on
// the agent's monotonic clock from the time it received the grant.
type grant struct {
	id        string
	sandboxID string
	kind      ownsandv1.RelayOperationKind
	opID      string
	digest    []byte
	gen       uint64
	received  time.Time
	expiresIn time.Duration
	// deadlineIn is the stream deadline duration.
	deadlineIn time.Duration
	streams    map[streamEnder]struct{}
	idleSince  time.Time
	opFinished bool
}

func (g *grant) target() GrantTarget {
	return GrantTarget{SandboxID: g.sandboxID, Kind: g.kind, OpID: g.opID, Digest: g.digest}
}

// admittable: not past its expiry plus the admission tolerance.
func (g *grant) admittable(now time.Time) bool {
	return now.Before(g.received.Add(g.expiresIn + AdmissionTolerance))
}

func (g *grant) expired(now time.Time) bool { return !now.Before(g.received.Add(g.expiresIn)) }

func (g *grant) pastDeadline(now time.Time) bool { return !now.Before(g.received.Add(g.deadlineIn)) }

// grantTable holds the relay grants of the current agent channel.
type grantTable struct {
	log  *slog.Logger
	now  func() time.Time
	send func(*ownsandv1.AgentEnvelope) bool

	mu     sync.Mutex
	gen    uint64
	grants map[string]*grant
}

func newGrantTable(log *slog.Logger, now func() time.Time) *grantTable {
	return &grantTable{log: log, now: now, grants: map[string]*grant{}}
}

func (t *grantTable) setChannel(gen uint64, send func(*ownsandv1.AgentEnvelope) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gen, t.send = gen, send
}

// install handles a relay grant from the agent channel and returns the
// acknowledgement.
func (t *grantTable) install(gen uint64, rg *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
	now := t.now()
	t.mu.Lock()
	ack := &ownsandv1.RelayGrantAck{GrantId: rg.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}
	if gen != t.gen {
		// A grant of an older channel is not bound to the current channel.
		t.mu.Unlock()
		ack.Result = ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED
		return ack
	}
	if _, ok := t.grants[rg.GetGrantId()]; ok {
		t.mu.Unlock()
		return ack
	}
	var replaced *grant
	if rg.ReplacesGrantId != nil {
		if old := t.grants[rg.GetReplacesGrantId()]; old != nil && old.gen == gen && !old.pastDeadline(now) {
			replaced = old
			delete(t.grants, old.id)
			id := old.id
			ack.ReplacedGrantId = &id
		}
	}
	if replaced == nil && len(t.grants) >= MaxGrants {
		t.mu.Unlock()
		ack.Result = ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT
		t.log.Warn("relay grant rejected at the per-device limit", "grant_id", rg.GetGrantId())
		return ack
	}
	g := &grant{
		id: rg.GetGrantId(), sandboxID: rg.GetSandboxId(), kind: rg.GetOperationKind(), opID: rg.GetOperationId(),
		digest: rg.GetRequestDigest(), gen: gen, received: now,
		expiresIn: rg.GetExpiresIn().AsDuration(), deadlineIn: rg.GetStreamDeadlineIn().AsDuration(),
		streams: map[streamEnder]struct{}{}, idleSince: now,
	}
	t.grants[g.id] = g
	var ended []streamEnder
	if replaced != nil {
		for s := range replaced.streams {
			ended = append(ended, s)
		}
	}
	t.mu.Unlock()
	for _, s := range ended {
		s.endWith(ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_REPLACED)
	}
	t.log.Info("relay grant installed", "grant_id", g.id, "sandbox_id", g.sandboxID, "operation_kind", g.kind.String(),
		"replaced_grant_id", ack.GetReplacedGrantId(), "expires_in", g.expiresIn.String())
	return ack
}

// count returns the number of grants that count against the limit.
func (t *grantTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.grants)
}

// hasLive reports whether a grant is admittable.
func (t *grantTable) hasLive() bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, g := range t.grants {
		if g.admittable(now) {
			return true
		}
	}
	return false
}

// openStream admits a relay stream open frame: the grant must be live,
// bound to the current channel and issued for the frame's sandbox.
func (t *grantTable) openStream(grantID, sandboxID string, s streamEnder, maxPerGrant int) (*grant, error) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	g := t.grants[grantID]
	if g == nil || g.gen != t.gen || g.sandboxID != sandboxID || !g.admittable(now) {
		return nil, errGrantInvalid
	}
	if len(g.streams) >= maxPerGrant {
		return nil, connect.NewError(connect.CodeResourceExhausted, errNoStreamCapacity)
	}
	g.streams[s] = struct{}{}
	return g, nil
}

// admitRequest checks that the grant is still live when the first request
// frame of a stream arrives.
func (t *grantTable) admitRequest(g *grant) bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.grants[g.id] == g && g.admittable(now)
}

// streamEnded removes a stream and applies the release rules.
func (t *grantTable) streamEnded(g *grant, s streamEnder, o Outcome, attachUnderExec bool) {
	now := t.now()
	t.mu.Lock()
	delete(g.streams, s)
	g.idleSince = now
	if o.Finished {
		g.opFinished = true
	}
	release := false
	if t.grants[g.id] == g && len(g.streams) == 0 {
		switch {
		case g.opFinished:
			release = true
		case !o.Started && releasingCode(o.Code) && !(attachUnderExec && o.Code == connect.CodeNotFound):
			// The request never started and the error leaves no operation to
			// continue. An AttachExec not_found under an EXEC grant keeps the
			// grant: the caller may follow it with the identical ExecRequest.
			release = true
		case g.expired(now):
			release = true
		}
	}
	if release {
		delete(t.grants, g.id)
	}
	send := t.send
	t.mu.Unlock()
	if release {
		t.released(g, send)
	}
}

func releasingCode(c connect.Code) bool {
	switch c {
	case connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeAlreadyExists, connect.CodeFailedPrecondition:
		return true
	}
	return false
}

func (t *grantTable) released(g *grant, send func(*ownsandv1.AgentEnvelope) bool) {
	t.log.Info("relay grant released", "grant_id", g.id)
	if send != nil {
		send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantRelease{RelayGrantRelease: &ownsandv1.RelayGrantRelease{GrantId: g.id}}})
	}
}

// sweep ends streams at the stream deadline and releases idle and expired
// grants.
func (t *grantTable) sweep() {
	now := t.now()
	t.mu.Lock()
	var ended []streamEnder
	var released []*grant
	for id, g := range t.grants {
		switch {
		case g.pastDeadline(now):
			for s := range g.streams {
				ended = append(ended, s)
			}
			delete(t.grants, id)
			released = append(released, g)
		case len(g.streams) == 0 && (now.Sub(g.idleSince) >= GrantIdleRelease || g.expired(now) || g.opFinished):
			delete(t.grants, id)
			released = append(released, g)
		}
	}
	send := t.send
	t.mu.Unlock()
	for _, s := range ended {
		s.endWith(ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
	}
	for _, g := range released {
		t.released(g, send)
	}
}

// revoke ends the streams of a grant with REVOKED and drops it.
func (t *grantTable) revoke(grantID string) {
	t.mu.Lock()
	g := t.grants[grantID]
	delete(t.grants, grantID)
	var ended []streamEnder
	if g != nil {
		for s := range g.streams {
			ended = append(ended, s)
		}
	}
	t.mu.Unlock()
	for _, s := range ended {
		s.endWith(ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
	}
	if g != nil {
		t.log.Info("relay grant revoked", "grant_id", grantID, "open_streams", len(ended))
	}
}

// channelLost ends every grant of the channel with AGENT_CHANNEL_LOST.
func (t *grantTable) channelLost(gen uint64) {
	t.mu.Lock()
	var ended []streamEnder
	for id, g := range t.grants {
		if g.gen != gen {
			continue
		}
		for s := range g.streams {
			ended = append(ended, s)
		}
		delete(t.grants, id)
	}
	if t.gen == gen {
		t.send = nil
	}
	t.mu.Unlock()
	for _, s := range ended {
		s.endWith(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
}
