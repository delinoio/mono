// SPDX-License-Identifier: FSL-1.1-ALv2

package webhook_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
)

func TestReadBody(t *testing.T) {
	for _, tc := range []struct {
		size int
		err  error
	}{{0, nil}, {webhook.MaxBodyBytes, nil}, {webhook.MaxBodyBytes + 1, webhook.ErrTooLarge}} {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/x", bytes.NewReader(make([]byte, tc.size)))
		body, err := webhook.ReadBody(httptest.NewRecorder(), r)
		if !errors.Is(err, tc.err) || (err == nil && len(body) != tc.size) {
			t.Errorf("size %d: %d bytes, %v", tc.size, len(body), err)
		}
	}
}

func TestRecordAndRetention(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tc := range []struct {
		sender webhook.Sender
		id     string
		want   bool
	}{
		{webhook.SenderGitHub, "d1", true},
		{webhook.SenderGitHub, "d1", false},
		// The same ID of another sender is another delivery.
		{webhook.SenderOpenAI, "d1", true},
	} {
		got, err := webhook.Record(ctx, d.Pool, tc.sender, tc.id, now.Add(-8*24*time.Hour))
		if err != nil || got != tc.want {
			t.Fatalf("record %v %s = %v, %v", tc.sender, tc.id, got, err)
		}
	}
	if _, err := webhook.Record(ctx, d.Pool, webhook.SenderGitHub, "d2", now); err != nil {
		t.Fatal(err)
	}
	n, err := webhook.RetentionJob(d).Run(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("retention deleted %d, %v", n, err)
	}
	if seen, _ := webhook.Seen(ctx, d.Pool, webhook.SenderGitHub, "d2"); !seen {
		t.Fatal("a recent delivery was deleted")
	}
	if seen, _ := webhook.Seen(ctx, d.Pool, webhook.SenderGitHub, "d1"); seen {
		t.Fatal("an old delivery was kept")
	}
	if webhook.SenderGitHub.Label() != "github" || webhook.SenderOpenAI.Label() != "openai" {
		t.Fatal("labels")
	}
}
