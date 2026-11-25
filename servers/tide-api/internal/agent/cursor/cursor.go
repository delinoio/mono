// SPDX-License-Identifier: FSL-1.1-ALv2

// Package cursor is the CURSOR vendor of the AGENT_SESSION workload: tide
// is the controller of a Cursor self-hosted pool. The lease holder lists
// and watches the pool's pending requests with the integration's service
// account API key, claims each request with a worker ID that tide chooses,
// creates the tide request, and starts one `agent worker` per claimed
// request in an OwnSand sandbox. The owning contract is the Cursor and Devin
// section of docs/servers-tide-api-foundation.md, which records the
// endpoints, the command line and the gaps of the Cursor documentation.
//
// Cursor API (https://cursor.com/docs/cloud-agent/self-hosted/pool and the
// Workers and Pools sections of https://cursor.com/docs/cloud-agent/api/endpoints):
//
//   - GET  /v0/private-workers/pending-requests?pool=&limit=&pageToken=
//   - GET  /v0/private-workers/pending-requests/stream?pool=&cursor= (SSE)
//   - POST /v0/private-workers/claim {id, workerId}
//   - POST /v0/private-workers/claims/{id}/release
//   - POST /v0/private-workers/claims/{id}/fail {message}
//   - GET  /v0/private-workers?workerIds=
package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/vendorhttp"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// DefaultBaseURL is the Cursor API origin.
const DefaultBaseURL = "https://api.cursor.com"

// Defaults of Options.
const (
	// DefaultRelistInterval: a stream cursor expires 5 minutes after the list
	// that issued it, and Cursor recommends listing again every 5 minutes
	// with jitter, so tide lists again after 4 minutes plus up to 30 seconds.
	DefaultRelistInterval = 4 * time.Minute
	relistJitter          = 30 * time.Second
	// DefaultIdleReleaseTimeout (recorded decision): the worker waits 10
	// minutes for follow-up messages after its session's turn ends, then
	// exits with code 0 and the runner ends. Cursor's default is 1 hour,
	// which would keep the runner's capacity for an idle session.
	DefaultIdleReleaseTimeout = 600
	// workerBatch is the largest workerIds list of one status call.
	workerBatch = 100
)

// Options configures the vendor.
type Options struct {
	// BaseURL is the Cursor API origin; empty selects DefaultBaseURL.
	BaseURL string
	// HTTP sends the API calls; nil selects http.DefaultClient.
	HTTP *http.Client
	// RelistInterval is the time after a list when the watch lists again;
	// zero selects DefaultRelistInterval (plus jitter).
	RelistInterval time.Duration
	// IdleReleaseTimeout is the worker's --idle-release-timeout in seconds;
	// zero selects DefaultIdleReleaseTimeout.
	IdleReleaseTimeout int
}

// Vendor is the Cursor vendor.
type Vendor struct {
	hub  *agent.Hub
	opts Options
	log  *slog.Logger
}

var _ agent.Vendor = (*Vendor)(nil)

// Register adds the Cursor vendor to the Hub.
func Register(hub *agent.Hub, opts Options) *Vendor {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.IdleReleaseTimeout == 0 {
		opts.IdleReleaseTimeout = DefaultIdleReleaseTimeout
	}
	v := &Vendor{hub: hub, opts: opts, log: hub.Service().Logger}
	hub.AddVendor(v)
	return v
}

func (*Vendor) Vendor() tidev1.AgentVendor { return tidev1.AgentVendor_AGENT_VENDOR_CURSOR }
func (*Vendor) Source() work.Source        { return work.SourceCursor }

// api returns a client with the integration's service account API key.
// The key is sent with Basic authentication (the key as user name and an
// empty password), as the Cursor examples do.
func (v *Vendor) api(ctx context.Context, in integration.Integration) (*vendorhttp.Client, error) {
	key, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY)
	if err != nil {
		return nil, err
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(key+":"))
	return &vendorhttp.Client{Vendor: "CURSOR", BaseURL: v.opts.BaseURL, HTTP: v.opts.HTTP, Logger: v.log,
		Auth: func(h http.Header) { h.Set("Authorization", basic) }}, nil
}

// PendingRequest is one entry of the pending request list or a `created`
// event.
type PendingRequest struct {
	ID              string `json:"id"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	ClaimedWorkerID string `json:"claimedWorkerId,omitempty"`
	WakeTimeoutMs   int64  `json:"wakeTimeoutMs,omitempty"`
}

type pendingPage struct {
	Requests      []PendingRequest `json:"requests"`
	NextPageToken string           `json:"nextPageToken"`
	StreamCursor  string           `json:"streamCursor"`
}

// Watch lists the pool's pending requests, admits each unclaimed one, and
// follows the stream from the list's cursor until the relist time
// (scheduled end), a `410 Gone` (cursor expired: scheduled end, list again
// at once) or another end (an error).
func (v *Vendor) Watch(ctx context.Context, w *agent.Watch) error {
	c, err := v.api(ctx, w.In)
	if err != nil {
		return err
	}
	pool := w.In.PoolName
	cursor := ""
	token := ""
	usedTokens := make(map[string]struct{})
	for {
		q := url.Values{"pool": {pool}, "limit": {"100"}}
		if token != "" {
			q.Set("pageToken", token)
			usedTokens[token] = struct{}{}
		}
		var page pendingPage
		if err := c.Do(ctx, "list_pending_requests", http.MethodGet, "/v0/private-workers/pending-requests", q, nil, &page); err != nil {
			return err
		}
		if _, used := usedTokens[page.NextPageToken]; used {
			return errors.New("the Cursor pending request list repeated a page token")
		}
		for _, r := range page.Requests {
			v.admit(ctx, w, c, r)
		}
		cursor = page.StreamCursor
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	relist := v.opts.RelistInterval
	if relist == 0 {
		relist = DefaultRelistInterval + rand.N(relistJitter)
	}
	sctx, cancel := context.WithTimeout(ctx, relist)
	defer cancel()
	q := url.Values{"pool": {pool}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err = c.Stream(sctx, "watch_pending_requests", "/v0/private-workers/pending-requests/stream", q, nil, func(e vendorhttp.Event) error {
		switch e.Name {
		case "created":
			var r PendingRequest
			if err := json.Unmarshal(e.Data, &r); err != nil || r.ID == "" {
				w.Logger.WarnContext(ctx, "vendor_event_invalid", slog.String("event", e.Name))
				return nil
			}
			v.admit(ctx, w, c, r)
		case "claimed", "claimed_offline", "expired", "heartbeat":
			// claimed: tide's own claims and those of other controllers.
			// claimed_offline: a claim whose worker is offline; tide does not
			// adopt it (see the contract). expired: the request left the queue
			// unclaimed, so tide holds no claim of it.
		}
		return nil
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded) && sctx.Err() != nil:
		return nil
	case vendorhttp.Status(err) == http.StatusGone:
		return nil
	case err == nil:
		return errors.New("the Cursor pending request stream ended")
	}
	return err
}

// admit claims one pending request. Entries with a claimed worker
// (claimed_offline) are not claimable.
func (v *Vendor) admit(ctx context.Context, w *agent.Watch, c *vendorhttp.Client, r PendingRequest) {
	if r.ClaimedWorkerID != "" {
		return
	}
	err := w.Admit(ctx, agent.Demand{
		VendorRequestID: r.ID, Marker: strconv.FormatInt(r.CreatedAtMs, 10), Scope: w.In.PoolName,
		Claim: func(ctx context.Context, workerID string) error {
			err := c.Do(ctx, "claim", http.MethodPost, "/v0/private-workers/claim", nil, map[string]any{"id": r.ID, "workerId": workerID}, nil)
			if vendorhttp.Status(err) == http.StatusConflict {
				// Another worker or controller holds the request (or tide's
				// earlier attempt committed; see the contract's gaps).
				return agent.ErrClaimTaken
			}
			return err
		},
	})
	if err != nil && ctx.Err() == nil {
		w.Logger.WarnContext(ctx, "agent_admit_failed", slog.String("vendor_request_id", r.ID), slog.Any("error", err))
	}
}

type workerStatus struct {
	Workers []struct {
		WorkerID      string `json:"workerId"`
		ConnectedAtMs int64  `json:"connectedAtMs"`
		IsInUse       bool   `json:"isInUse"`
	} `json:"workers"`
}

// CheckStarted asks Cursor for the status of the claims' workers: a worker
// that is connected or in use took its session.
func (v *Vendor) CheckStarted(ctx context.Context, in integration.Integration, claims []*agent.Claim) ([]string, error) {
	c, err := v.api(ctx, in)
	if err != nil {
		return nil, err
	}
	byWorker := map[string]string{}
	var ids []string
	for _, cl := range claims {
		byWorker[cl.WorkerID] = cl.RequestID
		ids = append(ids, cl.WorkerID)
	}
	var out []string
	for len(ids) > 0 {
		n := min(len(ids), workerBatch)
		var st workerStatus
		if err := c.Do(ctx, "get_workers", http.MethodGet, "/v0/private-workers", url.Values{"workerIds": {strings.Join(ids[:n], ",")}}, nil, &st); err != nil {
			return out, err
		}
		ids = ids[n:]
		for _, wk := range st.Workers {
			if req, ok := byWorker[wk.WorkerID]; ok && (wk.ConnectedAtMs > 0 || wk.IsInUse) {
				out = append(out, req)
			}
		}
	}
	return out, nil
}

// failMessage is the message that ends the waiting turn of a request that
// tide gave up before a worker took it (1 to 500 characters).
func failMessage(req *work.Request) string {
	switch req.EndReason {
	case tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT:
		return "tide could not start a worker before the maximum queue wait of the runner profile ended."
	case tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE:
		return "No device of the tide organization can run the runner profile of this pool."
	case tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED:
		return "tide could not start a worker for this request."
	case tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER:
		return "A tide admin canceled this request."
	case tidev1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED:
		return "The tide runner profile of this pool was deleted."
	case tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED:
		return "The tide organization no longer has an OwnSand device pool."
	}
	if !work.RequestTerminal(req.State) {
		return "The tide organization of this pool was deleted."
	}
	return "tide could not serve this request."
}

// Release gives up a claim:
//
//   - A request that tide gave up before a worker took it (no start time,
//     not canceled by the source) ends the user's waiting turn with `fail`
//     and a message. When Cursor answers that no turn waits or that the
//     worker is connected, tide releases the claim instead.
//   - Otherwise tide releases the claim, which returns a waiting follow-up
//     to the queue and tells a running worker to exit.
//
// `404` means that no live claim exists (released, expired or adopted) and
// counts as done; `400 Worker in use` and `5xx` keep the claim for a retry.
func (v *Vendor) Release(ctx context.Context, in integration.Integration, cl *agent.Claim, req *work.Request) (bool, error) {
	c, err := v.api(ctx, in)
	if err != nil {
		return false, err
	}
	path := "/v0/private-workers/claims/" + url.PathEscape(cl.VendorRequestID)
	if req.StartTime == nil && req.EndReason != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		err := c.Do(ctx, "fail_claim", http.MethodPost, path+"/fail", nil, map[string]string{"message": failMessage(req)}, nil)
		switch st := vendorhttp.Status(err); {
		case err == nil, st == http.StatusNotFound:
			return true, nil
		case st != http.StatusBadRequest:
			return false, err
		}
	}
	err = c.Do(ctx, "release_claim", http.MethodPost, path+"/release", nil, nil, nil)
	switch vendorhttp.Status(err) {
	case http.StatusNotFound:
		return true, nil
	case 0:
		return err == nil, err
	}
	return false, err
}

// Prepare checks for the Cursor `agent` CLI with `agent --version` and
// installs it with the official installer when it is missing. Windows
// guests need Git for Windows with Git Bash, without which the worker does
// not start.
func (v *Vendor) Prepare(ctx context.Context, g *work.Guest, _ integration.Integration) error {
	guest := g.Runner.GuestOS
	return agent.RunStep(ctx, g, "install the Cursor agent CLI", InstallScript(guest), agent.InstallTimeout, installCodes)
}

// Exit codes of InstallScript.
var installCodes = map[int64]string{
	10: "the guest has no curl, which the Cursor installer needs",
	11: "the guest has no bash, which the Cursor installer needs",
	12: "the Cursor installer failed",
	13: "the Cursor agent CLI is missing after the installation",
	14: "Cursor workers on Windows need Git for Windows with Git Bash in the guest image",
}

// InstallScript returns the guest script of Prepare.
func InstallScript(guest tidev1.GuestOs) string {
	if agent.IsWindows(guest) {
		return agent.ScriptPrelude(guest) + `$git = Get-Command git.exe -ErrorAction SilentlyContinue
$bash = $null
if ($git) { $c = Join-Path (Split-Path (Split-Path $git.Source)) 'bin\bash.exe'; if (Test-Path $c) { $bash = $c } }
if (-not $bash -and (Test-Path "$env:ProgramFiles\Git\bin\bash.exe")) { $bash = "$env:ProgramFiles\Git\bin\bash.exe" }
if (-not $bash) { exit 14 }
if (Get-Command agent -ErrorAction SilentlyContinue) { exit 0 }
try { irm 'https://cursor.com/install?win32=true' | iex } catch { exit 12 }
` + agent.ScriptPrelude(guest) + `if (-not (Get-Command agent -ErrorAction SilentlyContinue)) { exit 13 }
exit 0
`
	}
	return agent.ScriptPrelude(guest) + `if agent --version >/dev/null 2>&1; then exit 0; fi
command -v curl >/dev/null 2>&1 || exit 10
command -v bash >/dev/null 2>&1 || exit 11
curl https://cursor.com/install -fsS | bash || exit 12
agent --version >/dev/null 2>&1 || exit 13
`
}

// Start writes the service account API key to the runner's credential file
// and starts one pool worker for the claim:
//
//	CURSOR_API_KEY=<key> CURSOR_AGENT_WORKER_ID=<claim workerId> \
//	  agent worker --pool <pool> --worker-dir <home> --idle-release-timeout 600 start
//
// The options come before `start`, as the Cursor reference documents. The
// worker serves the one agent of the claim and exits when its claim is
// released or at the idle release timeout (exit code 0).
func (v *Vendor) Start(ctx context.Context, g *work.Guest, in integration.Integration, cl *agent.Claim) (work.StartResult, error) {
	key, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY)
	if err != nil {
		return work.StartResult{}, err
	}
	dir, err := agent.WriteCredential(ctx, g, key)
	if err != nil {
		return work.StartResult{}, err
	}
	return agent.StartProgram(ctx, g, Program(dir, cl, v.opts.IdleReleaseTimeout))
}

// Program returns the runner program of a claim.
func Program(dir string, cl *agent.Claim, idleRelease int) agent.Program {
	return agent.Program{
		Dir: dir, CredentialEnv: "CURSOR_API_KEY",
		Env: [][2]string{{"CURSOR_AGENT_WORKER_ID", cl.WorkerID}},
		Args: []string{"agent", "worker", "--pool", cl.Scope, "--worker-dir", agent.HomeArg,
			"--idle-release-timeout", strconv.Itoa(idleRelease), "start"},
	}
}
