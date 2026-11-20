// SPDX-License-Identifier: FSL-1.1-ALv2

// Package grants keeps the relay grants of the lease holder in memory:
// pending and installed grants, their authorization IDs, replacement,
// release, revocation matching and the per-device and per-principal limits.
// Grants are never persisted. The owning contract is the Data path
// signaling and relay section of docs/servers-ownsand-api-foundation.md.
package grants

import (
	"bytes"
	"errors"
	"sync"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// Grant timing and limits of the contract.
const (
	Lifetime           = 15 * time.Minute
	StreamDeadline     = 30 * time.Minute
	AdmissionTolerance = 30 * time.Second
	DeliveryMargin     = 60 * time.Second
	MaxPerDevice       = 256
	MaxPerPrincipal    = 32
)

// State is the life cycle state of a grant.
type State int

const (
	// Pending: sent to the agent, no acknowledgement yet.
	Pending State = iota + 1
	// Installed: the agent accepted it.
	Installed
)

// Request is an AuthorizeRelay after authorization.
type Request struct {
	AuthorizationID string
	OrgID           string
	SandboxID       string
	DeviceID        string
	ChannelGen      uint64
	Principal       auth.Principal
	Kind            ownsandv1.RelayOperationKind
	OperationID     string
	Digest          []byte
	ReplacesID      string
}

func (r Request) sameOperation(g *Grant) bool {
	return g.Principal == r.Principal && g.SandboxID == r.SandboxID && g.OrgID == r.OrgID && g.Kind == r.Kind &&
		g.OperationID == r.OperationID && bytes.Equal(g.Digest, r.Digest)
}

// Grant is one relay grant.
type Grant struct {
	Request
	ID             string
	State          State
	ExpireAt       time.Time
	StreamDeadline time.Time
	// AckAt is when the acknowledgement arrived; StreamDeadlineIn is the
	// duration sent with the grant.
	AckAt            time.Time
	StreamDeadlineIn time.Duration
	// Replaces is the predecessor that this grant replaces, until the
	// acknowledgement names the swap; Successor is the pending successor.
	Replaces  string
	Successor string
	// done is closed when the outcome (installed or rejected) is known.
	done     chan struct{}
	rejected bool
}

// Done is closed when the agent accepted or rejected the grant.
func (g *Grant) Done() <-chan struct{} { return g.done }

// Rejected reports whether the agent rejected the grant.
func (g *Grant) Rejected() bool { return g.rejected }

// Errors of Reserve.
var (
	ErrLimit         = errors.New("relay grant limit reached")
	ErrReuse         = errors.New("authorization_id reused with another request")
	ErrReplaceActive = errors.New("the grant already has a pending replacement")
)

// Registry holds the grants.
type Registry struct {
	Now func() time.Time

	mu     sync.Mutex
	byID   map[string]*Grant
	byAuth map[string]*Grant
}

// New returns an empty registry.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{Now: now, byID: map[string]*Grant{}, byAuth: map[string]*Grant{}}
}

// revocableLocked reports whether g still counts: until the later of its
// stream deadline plus the delivery margin and its acknowledgement time
// plus the stream deadline duration that was sent.
func (r *Registry) revocableLocked(g *Grant, now time.Time) bool {
	end := g.StreamDeadline.Add(DeliveryMargin)
	if !g.AckAt.IsZero() {
		if alt := g.AckAt.Add(g.StreamDeadlineIn); alt.After(end) {
			end = alt
		}
	}
	return now.Before(end)
}

// admittableLocked reports whether a new relay stream may start under g.
func admittable(g *Grant, now time.Time) bool {
	return g.State == Installed && now.Before(g.ExpireAt.Add(AdmissionTolerance))
}

func (r *Registry) sweepLocked(now time.Time) {
	for id, g := range r.byID {
		if !r.revocableLocked(g, now) || (g.State == Pending && !now.Before(g.ExpireAt.Add(AdmissionTolerance))) {
			r.removeLocked(id)
		}
	}
}

func (r *Registry) removeLocked(id string) {
	g := r.byID[id]
	if g == nil {
		return
	}
	delete(r.byID, id)
	if r.byAuth[g.AuthorizationID] == g {
		delete(r.byAuth, g.AuthorizationID)
	}
	if g.Replaces != "" {
		if p := r.byID[g.Replaces]; p != nil && p.Successor == id {
			p.Successor = ""
		}
	}
	if g.Successor != "" {
		if s := r.byID[g.Successor]; s != nil {
			s.Replaces = ""
		}
	}
	select {
	case <-g.done:
	default:
		g.rejected = true
		close(g.done)
	}
}

// countsLocked counts the slots in use per device and principal. A pending
// successor shares the slot of its predecessor.
func (r *Registry) countsLocked(deviceID string, p auth.Principal) (int, int) {
	var device, principal int
	for _, g := range r.byID {
		if g.Replaces != "" && r.byID[g.Replaces] != nil {
			continue
		}
		if g.DeviceID == deviceID {
			device++
		}
		if g.Principal == p {
			principal++
		}
	}
	return device, principal
}

// Reserve creates a pending grant for req, or returns the existing grant of
// a known authorization ID (existing is true). newID creates grant IDs.
func (r *Registry) Reserve(req Request, newID func() string) (g *Grant, existing bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	r.sweepLocked(now)
	if old := r.byAuth[req.AuthorizationID]; old != nil && old.State == Installed && !admittable(old, now) {
		// The grant of this authorization ID is no longer admittable: a retry
		// gets a new grant.
		delete(r.byAuth, req.AuthorizationID)
	}
	if old := r.byAuth[req.AuthorizationID]; old != nil {
		if !req.sameOperation(old) || old.DeviceID != req.DeviceID {
			return nil, false, ErrReuse
		}
		return old, true, nil
	}
	var predecessor *Grant
	if req.ReplacesID != "" {
		if p := r.byID[req.ReplacesID]; p != nil && req.sameOperation(p) && p.ChannelGen == req.ChannelGen && r.revocableLocked(p, now) {
			if p.Successor != "" {
				return nil, false, ErrReplaceActive
			}
			predecessor = p
		}
	}
	if predecessor == nil {
		device, principal := r.countsLocked(req.DeviceID, req.Principal)
		if device >= MaxPerDevice || principal >= MaxPerPrincipal {
			return nil, false, ErrLimit
		}
	}
	g = &Grant{
		Request: req, ID: newID(), State: Pending,
		ExpireAt: now.Add(Lifetime), StreamDeadline: now.Add(StreamDeadline), StreamDeadlineIn: StreamDeadline,
		done: make(chan struct{}),
	}
	if predecessor != nil {
		g.Replaces = predecessor.ID
		predecessor.Successor = g.ID
	} else {
		g.Request.ReplacesID = ""
	}
	r.byID[g.ID] = g
	r.byAuth[req.AuthorizationID] = g
	return g, false, nil
}

// Ack applies the agent's acknowledgement of grantID. An accepted
// replacement removes the predecessor that the agent named; a rejection
// removes the grant.
func (r *Registry) Ack(grantID string, result ownsandv1.RelayGrantResult, replacedID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.byID[grantID]
	if g == nil || g.State != Pending {
		return
	}
	if result != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		r.removeLocked(grantID)
		return
	}
	g.State = Installed
	g.AckAt = r.Now()
	if replacedID != "" && replacedID == g.Replaces {
		r.removeLocked(replacedID)
	}
	g.Replaces = ""
	close(g.done)
}

// Release removes a grant that the agent released.
func (r *Registry) Release(grantID, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g := r.byID[grantID]; g != nil && g.DeviceID == deviceID {
		r.removeLocked(grantID)
	}
}

// Admittable returns the grant when a new relay stream may start under it.
func (r *Registry) Admittable(grantID string) *Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.byID[grantID]
	if g == nil || !admittable(g, r.Now()) {
		return nil
	}
	return g
}

// Get returns a grant in any state, or nil.
func (r *Registry) Get(grantID string) *Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[grantID]
}

// RemoveChannel removes every grant bound to an agent channel generation of
// the device: the agent ends their streams when the channel closes.
func (r *Registry) RemoveChannel(deviceID string, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, g := range r.byID {
		if g.DeviceID == deviceID && g.ChannelGen == gen {
			r.removeLocked(id)
		}
	}
}

// RemoveMatching removes and returns the revocable grants that match.
func (r *Registry) RemoveMatching(match func(*Grant) bool) []*Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	var out []*Grant
	for id, g := range r.byID {
		if r.revocableLocked(g, now) && match(g) {
			cp := *g
			out = append(out, &cp)
			r.removeLocked(id)
		}
	}
	return out
}

// Len returns the number of grants (tests).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}
