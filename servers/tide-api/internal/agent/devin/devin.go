// SPDX-License-Identifier: FSL-1.1-ALv2

// Package devin is the DEVIN vendor of the AGENT_SESSION workload: tide is
// an orchestrator of a Devin outpost. The lease holder lists and watches
// the outpost's queue through the Devin Fleet API with the integration's
// API token (scope account.outposts.write), which never leaves tide,
// claims each pending session with an acceptor ID that tide chooses,
// creates the tide request, and starts one `devin worker start --session`
// per claimed session with the outpost token only. The owning contract is
// the Cursor and Devin section of docs/servers-tide-api-foundation.md,
// which records the endpoints, the command line and the gaps of the Devin
// documentation.
//
// Fleet API (https://docs.devin.ai/cloud/outposts/reference and
// https://docs.devin.ai/cloud/outposts/orchestration):
//
//   - GET  /opbeta/outposts (outpost name to ID)
//   - GET  /opbeta/outposts/devins?outpost=&phase=pending&first=&cursor=
//   - GET  /opbeta/outposts/devins?outpost=&watch=true&cursor= (SSE)
//   - GET  /opbeta/outposts/devins/{session_id}
//   - POST /opbeta/outposts/devins/{session_id}/claim {acceptor_id}
//   - POST /opbeta/outposts/devins/{session_id}/release {acceptor_id}
//   - POST /opbeta/outposts/devins/{session_id}/operator-message {message}
package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/vendorhttp"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// DefaultBaseURL is the Devin API origin.
const DefaultBaseURL = "https://api.devin.ai"

// DefaultReleaseGrace (recorded decision): after the worker exits, Devin
// asks the orchestrator to confirm that session_status is suspended or
// terminated before it releases the claim, because the status can lag the
// exit by a few seconds. tide releases the claim when the status confirms
// it, or 2 minutes after the request ended.
const DefaultReleaseGrace = 2 * time.Minute

// Options configures the vendor.
type Options struct {
	// BaseURL is the Devin API origin; empty selects DefaultBaseURL.
	BaseURL string
	// HTTP sends the API calls; nil selects http.DefaultClient.
	HTTP *http.Client
	// ReleaseGrace is the longest wait for a confirmed session end before
	// tide releases the claim; zero selects DefaultReleaseGrace.
	ReleaseGrace time.Duration
}

// Vendor is the Devin vendor.
type Vendor struct {
	hub  *agent.Hub
	opts Options
	log  *slog.Logger
}

var _ agent.Vendor = (*Vendor)(nil)

// Register adds the Devin vendor to the Hub.
func Register(hub *agent.Hub, opts Options) *Vendor {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.ReleaseGrace == 0 {
		opts.ReleaseGrace = DefaultReleaseGrace
	}
	v := &Vendor{hub: hub, opts: opts, log: hub.Service().Logger}
	hub.AddVendor(v)
	return v
}

func (*Vendor) Vendor() tidev1.AgentVendor { return tidev1.AgentVendor_AGENT_VENDOR_DEVIN }
func (*Vendor) Source() work.Source        { return work.SourceDevin }

// api returns a client with the integration's API token. The token stays in
// tide: it never enters a runner.
func (v *Vendor) api(ctx context.Context, in integration.Integration) (*vendorhttp.Client, error) {
	token, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_API_TOKEN)
	if err != nil {
		return nil, err
	}
	return &vendorhttp.Client{Vendor: "DEVIN", BaseURL: v.opts.BaseURL, HTTP: v.opts.HTTP, Logger: v.log, Auth: vendorhttp.Bearer(token)}, nil
}

// Entry is a queue entry of the Fleet API.
type Entry struct {
	Metadata struct {
		SessionID string          `json:"session_id"`
		OutpostID string          `json:"outpost_id"`
		CreatedAt json.RawMessage `json:"created_at"`
	} `json:"metadata"`
	Spec struct {
		Kind string `json:"kind"`
	} `json:"spec"`
	Status struct {
		Phase         string `json:"phase"`
		AcceptorID    string `json:"acceptor_id"`
		SessionStatus string `json:"session_status"`
	} `json:"status"`
}

// marker returns the demand marker of an entry: its creation time.
func (e *Entry) marker() string { return string(bytes.Trim(e.Metadata.CreatedAt, `"`)) }

type entryPage struct {
	Items       []Entry `json:"items"`
	Cursor      string  `json:"cursor"`
	HasNextPage bool    `json:"has_next_page"`
}

type watchEvent struct {
	Type   string `json:"type"`
	Object Entry  `json:"object"`
	Cursor string `json:"cursor"`
}

type outpostPage struct {
	Items []struct {
		Metadata struct {
			OutpostID string `json:"outpost_id"`
		} `json:"metadata"`
		Spec struct {
			Name string `json:"name"`
		} `json:"spec"`
	} `json:"items"`
	Cursor      string `json:"cursor"`
	HasNextPage bool   `json:"has_next_page"`
}

// outpostID resolves the integration's outpost_name: an outpost whose
// spec.name or metadata.outpost_id equals it. The Fleet API filters the
// queue by outpost ID only.
func (v *Vendor) outpostID(ctx context.Context, c *vendorhttp.Client, name string) (string, error) {
	cursor := ""
	for {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page outpostPage
		if err := c.Do(ctx, "list_outposts", http.MethodGet, "/opbeta/outposts", q, nil, &page); err != nil {
			return "", err
		}
		for _, o := range page.Items {
			if o.Spec.Name == name || o.Metadata.OutpostID == name {
				return o.Metadata.OutpostID, nil
			}
		}
		if !page.HasNextPage || page.Cursor == "" || page.Cursor == cursor {
			return "", fmt.Errorf("no Devin outpost is named %q", name)
		}
		cursor = page.Cursor
	}
}

// Watch resolves the outpost, lists its pending sessions, admits each one,
// and follows the watch stream from the list's cursor. Devin ends a watch
// stream after at most 5 minutes; that end is scheduled.
func (v *Vendor) Watch(ctx context.Context, w *agent.Watch) error {
	c, err := v.api(ctx, w.In)
	if err != nil {
		return err
	}
	outpost, err := v.outpostID(ctx, c, w.In.OutpostName)
	if err != nil {
		return err
	}
	cursor := ""
	listedPending := make(map[string]struct{})
	for {
		q := url.Values{"outpost": {outpost}, "phase": {"pending"}, "first": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page entryPage
		if err := c.Do(ctx, "list_queue", http.MethodGet, "/opbeta/outposts/devins", q, nil, &page); err != nil {
			return err
		}
		stalled := page.HasNextPage && (page.Cursor == "" || page.Cursor == cursor)
		for i := range page.Items {
			e := &page.Items[i]
			if e.Status.Phase == "pending" && e.matchesOutpost(outpost) {
				sid := e.Metadata.SessionID
				// A stalled page can repeat pending entries that tide already saw.
				if _, listed := listedPending[sid]; stalled && listed {
					continue
				}
				listedPending[sid] = struct{}{}
			}
			v.handle(ctx, w, c, outpost, "MODIFIED", e)
		}
		if stalled {
			w.Logger.WarnContext(ctx, "devin_queue_pagination_stalled", slog.String("outpost_id", outpost),
				slog.Bool("request_cursor_present", cursor != ""), slog.Bool("response_cursor_present", page.Cursor != ""))
			break
		}
		if page.Cursor != "" {
			cursor = page.Cursor
		}
		if !page.HasNextPage {
			break
		}
	}
	q := url.Values{"outpost": {outpost}, "watch": {"true"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err = c.Stream(ctx, "watch_queue", "/opbeta/outposts/devins", q, nil, func(e vendorhttp.Event) error {
		var ev watchEvent
		if err := json.Unmarshal(e.Data, &ev); err != nil || ev.Object.Metadata.SessionID == "" {
			w.Logger.WarnContext(ctx, "vendor_event_invalid", slog.String("event", e.Name))
			return nil
		}
		v.handle(ctx, w, c, outpost, ev.Type, &ev.Object)
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (e *Entry) matchesOutpost(outpost string) bool {
	return e.Metadata.OutpostID == "" || e.Metadata.OutpostID == outpost
}

// handle applies one queue entry state (at-least-once, so idempotent):
//
//   - a pending entry is admitted (claimed, or claimed again with the same
//     acceptor ID when tide's claim lapsed);
//   - an entry that tide's acceptor ID claimed and whose session runs means
//     that the worker took the session;
//   - an entry that another acceptor claimed, or a DELETED entry, ends a
//     QUEUED tide request with CANCELED_BY_SOURCE.
func (v *Vendor) handle(ctx context.Context, w *agent.Watch, c *vendorhttp.Client, outpost, typ string, e *Entry) {
	if !e.matchesOutpost(outpost) {
		return
	}
	sid := e.Metadata.SessionID
	var err error
	switch {
	case typ == "DELETED":
		err = w.Lost(ctx, sid)
	case e.Status.Phase == "pending":
		err = w.Admit(ctx, agent.Demand{VendorRequestID: sid, Marker: e.marker(), Scope: outpost,
			Claim: func(ctx context.Context, acceptorID string) error {
				err := c.Do(ctx, "claim", http.MethodPost, "/opbeta/outposts/devins/"+url.PathEscape(sid)+"/claim", nil, map[string]string{"acceptor_id": acceptorID}, nil)
				if vendorhttp.Status(err) == http.StatusConflict {
					return agent.ErrClaimTaken
				}
				return err
			}})
	case e.Status.Phase == "claimed":
		var cl *agent.Claim
		var req *work.Request
		cl, req, err = w.Claim(ctx, sid)
		if err != nil || cl == nil || work.RequestTerminal(req.State) {
			break
		}
		switch {
		case e.Status.AcceptorID != cl.WorkerID:
			err = w.Lost(ctx, sid)
		case e.Status.SessionStatus == "running":
			err = w.Taken(ctx, sid, cl.WorkerID)
		}
	}
	if err != nil && ctx.Err() == nil {
		w.Logger.WarnContext(ctx, "agent_admit_failed", slog.String("vendor_request_id", sid), slog.Any("error", err))
	}
}

func (v *Vendor) entry(ctx context.Context, c *vendorhttp.Client, sid string) (*Entry, error) {
	var e Entry
	if err := c.Do(ctx, "get_queue_entry", http.MethodGet, "/opbeta/outposts/devins/"+url.PathEscape(sid), nil, nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// CheckStarted reads the claims' queue entries: an entry that tide's
// acceptor ID holds and whose session runs means that the worker took it.
func (v *Vendor) CheckStarted(ctx context.Context, in integration.Integration, claims []*agent.Claim) ([]string, error) {
	c, err := v.api(ctx, in)
	if err != nil {
		return nil, err
	}
	var out []string
	var errs []error
	for _, cl := range claims {
		e, err := v.entry(ctx, c, cl.VendorRequestID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if e.Status.AcceptorID == cl.WorkerID && e.Status.SessionStatus == "running" {
			out = append(out, cl.RequestID)
		}
	}
	return out, errors.Join(errs...)
}

// operatorMessage is the banner that tide shows in a session that it gave
// up before a worker took it.
func operatorMessage(req *work.Request) string {
	switch req.EndReason {
	case tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT:
		return "tide could not start a worker before the maximum queue wait of the runner profile ended."
	case tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE:
		return "No device of the tide organization can run the runner profile of this outpost."
	case tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED:
		return "tide could not start a worker for this session."
	case tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER:
		return "A tide admin canceled the worker request of this session."
	}
	return "tide stopped serving this session."
}

// Release gives up a claim. For a session that a worker served, tide first
// confirms that session_status is suspended or terminated (or that the
// release grace passed). For a session that tide gave up before a worker
// took it, tide shows an operator message in the session first. The
// release returns the session to the queue at once. Devin documents no
// release status codes: a 2xx and a 404 count as done, other 4xx answers
// except 429 also count as done (the claim is not tide's), and 429 and
// 5xx keep the claim for a retry.
func (v *Vendor) Release(ctx context.Context, in integration.Integration, cl *agent.Claim, req *work.Request) (bool, error) {
	c, err := v.api(ctx, in)
	if err != nil {
		return false, err
	}
	sid := cl.VendorRequestID
	base := "/opbeta/outposts/devins/" + url.PathEscape(sid)
	if req.StartTime != nil {
		e, err := v.entry(ctx, c, sid)
		switch {
		case vendorhttp.Status(err) == http.StatusNotFound:
			return true, nil
		case err != nil:
			return false, err
		case e.Status.AcceptorID != cl.WorkerID:
			return true, nil
		case e.Status.SessionStatus != "suspended" && e.Status.SessionStatus != "terminated" &&
			(req.EndTime == nil || v.hub.Service().NowUTC().Sub(*req.EndTime) < v.opts.ReleaseGrace):
			return false, nil
		}
	} else if req.EndReason != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		if err := c.Do(ctx, "operator_message", http.MethodPost, base+"/operator-message", nil, map[string]string{"message": operatorMessage(req)}, nil); err != nil {
			v.log.WarnContext(ctx, "devin_operator_message_failed", slog.String("request_id", req.ID), slog.Any("error", err))
		}
	}
	err = c.Do(ctx, "release", http.MethodPost, base+"/release", nil, map[string]string{"acceptor_id": cl.WorkerID}, nil)
	st := vendorhttp.Status(err)
	switch {
	case err == nil:
		return true, nil
	case st == http.StatusTooManyRequests || st >= 500 || st == 0:
		return false, err
	}
	return true, nil
}

// Exit codes of InstallScript.
var installCodes = map[int64]string{
	10: "the guest has no curl, which the Devin installer needs",
	11: "the guest has no bash, which the Devin installer needs",
	12: "the Devin installer failed",
	13: "the devin CLI is missing after the installation",
	14: "Devin workers need git on PATH in the guest image",
	15: "Devin workers support macOS on arm64 and Windows on x64 only",
}

// InstallScript returns the guest script of Prepare: the git and platform
// checks, then the official Devin CLI installer when `devin` is missing.
func InstallScript(guest tidev1.GuestOs) string {
	if agent.IsWindows(guest) {
		return agent.ScriptPrelude(guest) + `if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64') { exit 15 }
if (-not (Get-Command git.exe -ErrorAction SilentlyContinue)) { exit 14 }
if (Get-Command devin -ErrorAction SilentlyContinue) { exit 0 }
try { irm https://static.devin.ai/cli/setup.ps1 | iex } catch { exit 12 }
` + agent.ScriptPrelude(guest) + `if (-not (Get-Command devin -ErrorAction SilentlyContinue)) { exit 13 }
exit 0
`
	}
	return agent.ScriptPrelude(guest, devinBin) + `command -v git >/dev/null 2>&1 || exit 14
if [ "$(uname -s)" = Darwin ] && [ "$(uname -m)" != arm64 ]; then exit 15; fi
if command -v devin >/dev/null 2>&1; then exit 0; fi
command -v curl >/dev/null 2>&1 || exit 10
command -v bash >/dev/null 2>&1 || exit 11
curl -fsSL https://cli.devin.ai/install.sh | bash || exit 12
command -v devin >/dev/null 2>&1 || exit 13
`
}

// devinBin is an extra bin directory relative to $HOME that the start and
// install scripts search, because Devin does not document the install path.
const devinBin = ".devin/bin"

// Prepare checks git and the platform, and installs the devin CLI when it
// is missing.
func (v *Vendor) Prepare(ctx context.Context, g *work.Guest, _ integration.Integration) error {
	return agent.RunStep(ctx, g, "install the Devin CLI", InstallScript(g.Runner.GuestOS), agent.InstallTimeout, installCodes)
}

// Start writes the outpost token to the runner's credential file and
// starts the worker for the claimed session, as the orchestration guide
// documents for an orchestrator that claims first:
//
//	DEVIN_OUTPOSTS_TOKEN=<outpost token> \
//	  devin worker start --session=<session_id> --outpost=<outpost_id> --acceptor-id=<claim acceptor_id>
//
// The worker serves that session and exits.
func (v *Vendor) Start(ctx context.Context, g *work.Guest, in integration.Integration, cl *agent.Claim) (work.StartResult, error) {
	token, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_OUTPOST_TOKEN)
	if err != nil {
		return work.StartResult{}, err
	}
	dir, err := agent.WriteCredential(ctx, g, token)
	if err != nil {
		return work.StartResult{}, err
	}
	return agent.StartProgram(ctx, g, Program(dir, cl))
}

// Program returns the runner program of a claim.
func Program(dir string, cl *agent.Claim) agent.Program {
	return agent.Program{
		Dir: dir, CredentialEnv: "DEVIN_OUTPOSTS_TOKEN", ExtraBins: []string{devinBin},
		Args: []string{"devin", "worker", "start", "--session=" + cl.VendorRequestID, "--outpost=" + cl.Scope, "--acceptor-id=" + cl.WorkerID},
	}
}
