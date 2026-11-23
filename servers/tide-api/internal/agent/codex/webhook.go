// SPDX-License-Identifier: FSL-1.1-ALv2

package codex

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/webhook"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// SignatureTolerance is the accepted difference between webhook-timestamp
// and the server time, in both directions: the default of the official
// OpenAI SDKs (the guide states no number).
const SignatureTolerance = 5 * time.Minute

// maxHeaderID bounds webhook-id and the session ID of a payload.
const maxHeaderID = 200

// errSignature is a missing or invalid signature.
var errSignature = errors.New("invalid webhook signature")

// SecretKey returns the HMAC key of a webhook secret, as the Standard
// Webhooks specification and the OpenAI SDKs define it: the base64
// (standard alphabet) decoding of the part after `whsec_`, or, for a
// secret without the prefix, the secret's bytes.
func SecretKey(secret string) ([]byte, error) {
	rest, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		return []byte(secret), nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(rest)
	if err != nil || len(key) == 0 {
		return nil, errors.New("the webhook secret is not whsec_ followed by base64")
	}
	return key, nil
}

// Sign returns the v1 signature of a delivery: base64 of HMAC-SHA256 over
// `<webhook-id>.<webhook-timestamp>.<body>`.
func Sign(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a delivery as the Standard Webhooks specification
// describes it: the webhook-id, webhook-timestamp and webhook-signature
// headers are required, the timestamp is within SignatureTolerance of now,
// and one of the space-separated `v1,<signature>` entries matches
// (several entries allow a secret rotation). Comparisons are constant-time.
func VerifySignature(key []byte, h http.Header, body []byte, now time.Time) error {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return errSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return errSignature
	}
	want, _ := base64.StdEncoding.DecodeString(Sign(key, id, ts, body))
	for _, entry := range strings.Fields(sigs) {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errSignature
}

// event is the part of a webhook payload that tide reads.
type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		ID             string `json:"id"`
		RequiredAction *struct {
			Type string `json:"type"`
		} `json:"required_action"`
	} `json:"data"`
}

// WebhookHandler serves POST /webhooks/openai/{integration_id}.
type WebhookHandler struct{ v *Vendor }

// outcome is what one delivery did, for the answer, the metric and the log.
type outcome struct {
	result                         webhook.Result
	eventType                      string
	integrationID, organizationID  string
	sessionID, requestID, runnerID string
	detail                         string
	after                          []func(context.Context)
}

func (o *outcome) ignore(detail string) {
	o.result, o.detail = webhook.ResultIgnored, detail
}

// safe keeps printable ASCII of a value for the log, at most 200 bytes.
func safe(v string) string {
	if len(v) > maxHeaderID {
		v = v[:maxHeaderID]
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if c := v[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func validID(v string) bool { return v != "" && len(v) <= maxHeaderID && safe(v) == v }

// ServeHTTP handles one delivery. It verifies the signature before it
// parses the body, records the webhook-id with the delivery's effects in
// one transaction and answers 200 after the commit. The session lookup
// runs before the transaction; a failed lookup answers 500 and records
// nothing, so OpenAI delivers the event again.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v := h.v
	svc := v.hub.Service()
	ctx := r.Context()
	o := &outcome{}
	status := http.StatusOK
	deliveryID := r.Header.Get("webhook-id")
	defer func() {
		v.m.WebhookDelivery(webhook.SenderOpenAI.Label(), string(o.result))
		level := slog.LevelInfo
		if status >= 400 {
			level = slog.LevelWarn
		}
		svc.Logger.LogAttrs(ctx, level, "openai_webhook_delivery", slog.String("delivery_id", safe(deliveryID)), slog.String("event", safe(o.eventType)),
			slog.String("result", string(o.result)), slog.Int("status", status), slog.String("agent_integration_id", o.integrationID),
			slog.String("organization_id", o.organizationID), slog.String("vendor_request_id", safe(o.sessionID)), slog.String("request_id", o.requestID),
			slog.String("runner_id", o.runnerID), slog.String("detail", o.detail))
	}()
	answer := func(code int, result webhook.Result) {
		status, o.result = code, result
		w.WriteHeader(code)
	}
	body, err := webhook.ReadBody(w, r)
	if errors.Is(err, webhook.ErrTooLarge) {
		answer(http.StatusRequestEntityTooLarge, webhook.ResultTooLarge)
		return
	}
	if err != nil {
		answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
		return
	}
	// The integration and its secret come from the URL; an unknown
	// integration has no secret that could verify the delivery, so it gets
	// the same answer as a wrong signature.
	id, err := api.NormalizeID("integration_id", r.PathValue("integration_id"))
	if err != nil {
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	in, err := v.hub.Integrations().GetByID(ctx, svc.DB.Pool, id)
	if err != nil || in.Vendor != tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX {
		if err != nil && !isNotFound(err) {
			svc.Logger.ErrorContext(ctx, "openai_webhook_failed", slog.Any("error", err))
			answer(http.StatusInternalServerError, webhook.ResultFailed)
			return
		}
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	o.integrationID, o.organizationID = in.ID, in.OrganizationID
	secret, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_WEBHOOK_SECRET)
	if err != nil {
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	key, err := SecretKey(secret)
	if err != nil {
		o.detail = "webhook secret format"
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	if err := VerifySignature(key, r.Header, body, svc.NowUTC()); err != nil {
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	var ev event
	if !validID(deliveryID) || json.Unmarshal(body, &ev) != nil || ev.Type == "" {
		answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
		return
	}
	o.eventType = ev.Type
	if seen, err := webhook.Seen(ctx, svc.DB.Pool, webhook.SenderOpenAI, deliveryID); err == nil && seen {
		answer(http.StatusOK, webhook.ResultDuplicate)
		return
	}
	var sess *Session
	switch {
	case ev.Type == "agent.session.action_required" && ev.Data.RequiredAction != nil && ev.Data.RequiredAction.Type == "environment_connection",
		ev.Type == "agent.session.failed":
		if !validID(ev.Data.ID) {
			answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
			return
		}
		o.sessionID = ev.Data.ID
		lctx, cancel := context.WithTimeout(ctx, v.opts.LookupTimeout)
		sess, err = v.session(lctx, in, ev.Data.ID)
		cancel()
		if err != nil && !errors.Is(err, errSessionGone) {
			if ctx.Err() == nil {
				svc.Logger.WarnContext(ctx, "openai_session_lookup_failed", slog.String("agent_integration_id", in.ID), slog.Any("error", err))
			}
			answer(http.StatusInternalServerError, webhook.ResultFailed)
			return
		}
	}
	err = svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		o.after, o.requestID, o.runnerID, o.detail = nil, "", "", ""
		isNew, err := webhook.Record(ctx, tx, webhook.SenderOpenAI, deliveryID, svc.NowUTC())
		if err != nil {
			return err
		}
		if !isNew {
			o.result = webhook.ResultDuplicate
			return nil
		}
		o.result = webhook.ResultProcessed
		switch {
		case ev.Type == "agent.session.failed":
			return v.sessionFailed(ctx, tx, in, ev.Data.ID, sess, o)
		case o.sessionID != "":
			return v.connectionRequired(ctx, tx, in, sess, o)
		}
		o.ignore("event not handled")
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			svc.Logger.ErrorContext(ctx, "openai_webhook_failed", slog.String("delivery_id", safe(deliveryID)), slog.Any("error", err))
		}
		answer(http.StatusInternalServerError, webhook.ResultFailed)
		return
	}
	answer(http.StatusOK, o.result)
	for _, fn := range o.after {
		fn(context.WithoutCancel(ctx))
	}
}

func isNotFound(err error) bool { return connect.CodeOf(err) == connect.CodeNotFound }

// connectionRequired queues a request for a session that still needs an
// executor of its self-hosted environment. The request key is
// `<session ID>:<environment ID>`; a later connection request of the same
// session after the earlier request ended gets `#<n>`.
func (v *Vendor) connectionRequired(ctx context.Context, tx pgx.Tx, in integration.Integration, sess *Session, o *outcome) error {
	if sess == nil {
		o.ignore("session deleted")
		return nil
	}
	if !sess.NeedsConnection() {
		o.ignore("no environment connection required")
		return nil
	}
	if !validID(sess.Environment.ID) || !validRemote(sess.Environment.RemoteURL) {
		o.ignore("invalid environment")
		return nil
	}
	l, ok, err := v.hub.Engine().Links().Get(ctx, tx, in.OrganizationID)
	if err != nil {
		return err
	}
	if !ok || !l.Dispatchable() {
		o.ignore("no active OwnSand link")
		return nil
	}
	ws := sess.Environment.WorkspaceDirectory
	if ws != "" && !validWorkspace(ws) {
		o.ignore("invalid workspace directory")
		return nil
	}
	r, created, err := v.hub.CreateDirectTx(ctx, tx, v, in, agent.DirectDemand{
		VendorRequestID: o.sessionID, SourceKey: o.sessionID + ":" + sess.Environment.ID, Repeat: true,
		Scope: sess.Environment.ID, Endpoint: sess.Environment.RemoteURL, WorkspaceDirectory: ws,
	})
	if errors.Is(err, agent.ErrNoProfile) {
		o.ignore("no runner profile")
		return nil
	}
	if err != nil {
		return err
	}
	o.requestID = r.ID
	if !created {
		o.ignore("open request exists")
		return nil
	}
	orgID := in.OrganizationID
	o.after = append(o.after, func(context.Context) { v.hub.Service().WorkChanged(orgID) })
	return nil
}

// validRemote reports whether a remote URL is an absolute https URL of at
// most 2048 bytes of printable ASCII without spaces. It does not use safe,
// whose 200-byte log limit would refuse longer valid URLs.
func validRemote(u string) bool {
	if !strings.HasPrefix(u, "https://") || len(u) > 2048 || len(u) <= len("https://") {
		return false
	}
	for i := 0; i < len(u); i++ {
		if c := u[i]; c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// sessionFailed releases the compute of a session that is still failed:
// a QUEUED request ends with CANCELED and CANCELED_BY_SOURCE, a started one
// with FAILED and CANCELED_BY_SOURCE, and its runner is deleted.
func (v *Vendor) sessionFailed(ctx context.Context, tx pgx.Tx, in integration.Integration, sessionID string, sess *Session, o *outcome) error {
	if sess == nil || sess.Status != "failed" {
		o.ignore("session not failed")
		return nil
	}
	c, req, err := v.hub.LatestClaim(ctx, tx, in, sessionID)
	if err != nil {
		return err
	}
	if c == nil || work.RequestTerminal(req.State) {
		o.ignore("no open request")
		return nil
	}
	o.requestID = req.ID
	engine := v.hub.Engine()
	end := work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_FAILED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE}
	if req.State == tidev1.RequestState_REQUEST_STATE_QUEUED {
		end.State = tidev1.RequestState_REQUEST_STATE_CANCELED
	}
	if _, err := engine.EndRequestTx(ctx, tx, req.ID, end); err != nil {
		return err
	}
	if req.RunnerID != "" {
		d, err := engine.DeleteRunnerTx(ctx, tx, req.OrganizationID, req.RunnerID, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP, 0)
		if err != nil && !isNotFound(err) {
			return err
		}
		o.runnerID = req.RunnerID
		o.after = append(o.after, func(ctx context.Context) { engine.FinishDeletion(ctx, d) })
	}
	orgID := in.OrganizationID
	o.after = append(o.after, func(context.Context) { v.hub.Service().WorkChanged(orgID) })
	return nil
}
