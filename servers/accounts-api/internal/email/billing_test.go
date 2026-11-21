// SPDX-License-Identifier: FSL-1.1-ALv2

package email

import (
	"strings"
	"testing"
)

func TestNoticeMessage(t *testing.T) {
	msg, err := NoticeMessage(Notice{
		Kind:               KindLowBalance,
		BillingAccountID:   "acct-1",
		BillingAccountName: `<b>Team</b>`,
		To:                 "owner@example.com",
		Link:               BillingAccountPath("https://accounts.example.test/", "acct-1"),
		Lines:              []string{"The credit balance is $5.00, below the alert threshold of $10.00."},
		IdempotencyKey:     "low_balance/acct-1/1/owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != KindLowBalance || msg.IdempotencyKey != "low_balance/acct-1/1/owner" || msg.BillingAccountID != "acct-1" {
		t.Fatalf("message = %+v", msg)
	}
	if !strings.Contains(msg.Text, "https://accounts.example.test/billing/acct-1") || !strings.Contains(msg.Text, "$5.00") {
		t.Fatalf("text = %s", msg.Text)
	}
	if strings.Contains(msg.HTML, "<b>Team</b>") || !strings.Contains(msg.HTML, "&lt;b&gt;Team&lt;/b&gt;") {
		t.Fatalf("html does not escape the name: %s", msg.HTML)
	}
	if _, err := NoticeMessage(Notice{Kind: KindInvitation}); err == nil {
		t.Fatal("invitation kind accepted as a notice")
	}
}
