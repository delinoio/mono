// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

func TestNotificationResendRetry(t *testing.T) {
	for _, test := range []struct {
		name        string
		firstStatus int
		firstBody   string
	}{
		{"missing acknowledgement", http.StatusOK, "null"},
		{"concurrent conflict", http.StatusConflict, `{"name":"concurrent_idempotent_requests","message":"private@example.com"}`},
		{"unknown conflict", http.StatusConflict, `{"name":"unknown","message":"private@example.com"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			account := f.account()
			id, key := f.queueNotice(account)
			first, second := f.resendRecipients(account)
			provider := &notificationProvider{responses: map[string][]providerResponse{
				key + "/" + first: {{test.firstStatus, test.firstBody}},
			}}
			logs := f.useNotificationProvider(provider)
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			if st := f.noticeState(id); st.Sent || st.Abandoned || st.Attempts != 1 || !slices.Equal(st.Recipients, []string{second}) {
				t.Fatalf("first pass state = %+v", st)
			}
			f.clock.Advance(time.Minute)
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			if st := f.noticeState(id); !st.Sent || st.Attempts != 2 || !slices.Equal(st.Recipients, []string{first, second}) {
				t.Fatalf("retry state = %+v", st)
			}
			provider.assertKeys(t, key+"/"+first, key+"/"+second, key+"/"+first)
			if strings.Contains(logs.String(), "private@example.com") || strings.Contains(logs.String(), "recipient_skipped") {
				t.Fatal("retry logged private provider text or a terminal skip")
			}
		})
	}
}

func TestNotificationResendReceiptRecovery(t *testing.T) {
	for _, test := range []struct {
		change     string
		laterFails bool
	}{
		{change: "unchanged"},
		{change: "account renamed", laterFails: true},
		{change: "primary email changed"},
		{change: "member demoted"},
	} {
		t.Run(test.change, func(t *testing.T) {
			f := newFixture(t)
			account := f.account()
			id, key := f.queueNotice(account)
			first, second := f.resendRecipients(account)
			firstKey, secondKey := key+"/"+first, key+"/"+second
			provider := &notificationProvider{responses: make(map[string][]providerResponse)}
			if test.laterFails {
				provider.responses[secondKey] = []providerResponse{{http.StatusServiceUnavailable, `{"message":"private@example.com"}`}}
			}
			logs := f.useNotificationProvider(provider)
			f.execResendSQL("ALTER TABLE billing_notification_deliveries ADD CONSTRAINT test_receipt_write CHECK (false)")
			if err := f.s.DeliverNotifications(ctx()); err == nil || !strings.Contains(err.Error(), "test_receipt_write") {
				t.Fatalf("first pass error = %v", err)
			}
			if st := f.noticeState(id); st.Sent || st.Attempts != 0 || len(st.Recipients) != 0 {
				t.Fatalf("state after failed receipt = %+v", st)
			}
			f.execResendSQL("ALTER TABLE billing_notification_deliveries DROP CONSTRAINT test_receipt_write")
			switch test.change {
			case "account renamed":
				f.execResendSQL("UPDATE billing_accounts SET name = 'Renamed' WHERE billing_account_id = $1", account)
			case "primary email changed":
				f.dir.Set(first, directory.User{PrimaryEmail: "changed@example.com"})
			case "member demoted":
				f.execResendSQL("UPDATE billing_account_members SET role = 'viewer' WHERE billing_account_id = $1 AND user_id = $2", account, first)
			}
			f.clock.Advance(notificationLease)
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{firstKey, firstKey, secondKey}
			wantRecipients := []string{first, second}
			if test.change == "member demoted" {
				wantKeys = []string{firstKey, secondKey}
				wantRecipients = []string{second}
			}
			if test.laterFails {
				if st := f.noticeState(id); st.Sent || st.Abandoned || st.Attempts != 1 || !slices.Equal(st.Recipients, []string{first}) {
					t.Fatalf("state after later recipient failed = %+v", st)
				}
				if !strings.Contains(logs.String(), `"failed_recipients":1`) {
					t.Fatal("only the other recipient should contribute to the failure")
				}
				f.clock.Advance(time.Minute)
				if err := f.s.DeliverNotifications(ctx()); err != nil {
					t.Fatal(err)
				}
				wantKeys = append(wantKeys, secondKey)
			} else if strings.Contains(logs.String(), "billing_notification_failed") {
				t.Fatal("the recovery pass should not record a failure")
			}
			wantAttempts := 1
			if test.laterFails {
				wantAttempts++
			}
			if st := f.noticeState(id); !st.Sent || st.Abandoned || st.Attempts != wantAttempts || !slices.Equal(st.Recipients, wantRecipients) {
				t.Fatalf("final state = %+v", st)
			}
			provider.assertKeys(t, wantKeys...)
			log := logs.String()
			wantConflict := test.change == "account renamed" || test.change == "primary email changed"
			if strings.Contains(log, `"reason":"idempotency_payload_conflict"`) != wantConflict {
				t.Fatalf("conflict diagnostic does not match recovery for %q", test.change)
			}
			wantFirstSendLogs := 1
			if test.change == "unchanged" {
				wantFirstSendLogs++
			}
			if strings.Count(log, `"resend_email_id":"email-1"`) != wantFirstSendLogs ||
				strings.Contains(log, "private@example.com") || strings.Contains(log, "changed@example.com") {
				t.Fatal("unexpected first-recipient acknowledgements or private text in logs")
			}
			provider.mu.Lock()
			accepted := len(provider.accepted)
			provider.mu.Unlock()
			if accepted != 2 {
				t.Fatalf("provider accepted %d distinct requests, want one per recipient", accepted)
			}
		})
	}
}

type providerResponse struct {
	status int
	body   string
}

// notificationProvider remembers the request accepted for each key and replays
// its ID when the same payload returns.
type notificationProvider struct {
	mu        sync.Mutex
	responses map[string][]providerResponse
	accepted  map[string]acceptedNotice
	keys      []string
}

type acceptedNotice struct {
	payload []byte
	body    string
}

func (p *notificationProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "could not read request", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, key)
	w.Header().Set("Content-Type", "application/json")
	if responses := p.responses[key]; len(responses) > 0 {
		p.responses[key] = responses[1:]
		w.WriteHeader(responses[0].status)
		_, _ = io.WriteString(w, responses[0].body)
		return
	}
	if previous, ok := p.accepted[key]; ok {
		if bytes.Equal(payload, previous.payload) {
			_, _ = io.WriteString(w, previous.body)
		} else {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"name":"invalid_idempotent_request","message":"private@example.com"}`)
		}
		return
	}
	if p.accepted == nil {
		p.accepted = make(map[string]acceptedNotice)
	}
	body := fmt.Sprintf(`{"id":"email-%d"}`, len(p.accepted)+1)
	p.accepted[key] = acceptedNotice{payload, body}
	_, _ = io.WriteString(w, body)
}

func (p *notificationProvider) assertKeys(t *testing.T, want ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(p.keys, want) {
		t.Fatalf("provider request keys = %v, want %v", p.keys, want)
	}
}

func (f *fixture) useNotificationProvider(provider http.Handler) *bytes.Buffer {
	f.t.Helper()
	srv := httptest.NewServer(provider)
	f.t.Cleanup(srv.Close)
	logs := new(bytes.Buffer)
	f.s.logger = slog.New(slog.NewJSONHandler(logs, nil))
	sender, err := email.NewResend(email.ResendConfig{
		APIKey: "test-key", From: "billing@example.com", BaseURL: srv.URL, Logger: f.s.logger,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.email = sender
	return logs
}

func (f *fixture) resendRecipients(account string) (first, second string) {
	f.t.Helper()
	if err := f.db.Pool.QueryRow(ctx(), `SELECT min(user_id), max(user_id)
		FROM billing_account_members WHERE billing_account_id = $1 AND role IN ('owner', 'admin')`, account).Scan(&first, &second); err != nil {
		f.t.Fatal(err)
	}
	return first, second
}

func (f *fixture) execResendSQL(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(ctx(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
