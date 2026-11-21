// SPDX-License-Identifier: FSL-1.1-ALv2

package email

import (
	"fmt"
	"html/template"
	"net/url"
	"strings"
	ttemplate "text/template"
)

// Kinds of billing notification emails (docs/servers-accounts-api-foundation.md,
// Emails). They go to the owners and admins of a billing account.
const (
	KindPaymentFailed     Kind = "payment_failed"
	KindAutoTopUpDisabled Kind = "auto_top_up_disabled"
	KindLowBalance        Kind = "low_balance"
	KindCreditExpiry      Kind = "credit_expiry"
)

// BillingAccountPath returns the web app page of a billing account.
func BillingAccountPath(webOrigin, billingAccountID string) string {
	return strings.TrimRight(webOrigin, "/") + "/billing/" + url.PathEscape(billingAccountID)
}

// Notice describes one billing notification email to one recipient. The
// amounts and times are already formatted for display.
type Notice struct {
	Kind               Kind
	BillingAccountID   string
	BillingAccountName string
	To                 string
	// Link is the billing account page of the web app.
	Link string
	// Lines are the plain sentences of the body, in order. They hold only
	// amounts, times and fixed text; html/template escapes them.
	Lines          []string
	IdempotencyKey string
}

type noticeData struct {
	Name  string
	Lines []string
	Link  string
}

var noticeText = ttemplate.Must(ttemplate.New("text").Parse(`Billing account "{{.Name}}"

{{range .Lines}}{{.}}
{{end}}
Manage billing:
{{.Link}}

You receive this email as an owner or admin of the billing account.
`))

var noticeHTML = template.Must(template.New("html").Parse(`<!doctype html>
<html lang="en">
<body style="font-family: sans-serif; line-height: 1.5;">
<p>Billing account <strong>{{.Name}}</strong></p>
{{range .Lines}}<p>{{.}}</p>
{{end}}<p><a href="{{.Link}}">Manage billing</a></p>
<p>You receive this email as an owner or admin of the billing account.</p>
</body>
</html>
`))

// subjects of the notification kinds.
var subjects = map[Kind]string{
	KindPaymentFailed:     "A Delino payment failed",
	KindAutoTopUpDisabled: "Delino automatic top-up is turned off",
	KindLowBalance:        "Your Delino credit balance is low",
	KindCreditExpiry:      "Delino credit expires in 7 days",
}

// NoticeMessage renders a billing notification email. html/template escapes
// the billing account name, which members chose.
func NoticeMessage(n Notice) (Message, error) {
	subject, ok := subjects[n.Kind]
	if !ok {
		return Message{}, fmt.Errorf("unknown notice kind %q", n.Kind)
	}
	data := noticeData{Name: n.BillingAccountName, Lines: n.Lines, Link: n.Link}
	var text, html strings.Builder
	if err := noticeText.Execute(&text, data); err != nil {
		return Message{}, err
	}
	if err := noticeHTML.Execute(&html, data); err != nil {
		return Message{}, err
	}
	return Message{
		Kind:             n.Kind,
		To:               n.To,
		Subject:          subject,
		Text:             text.String(),
		HTML:             html.String(),
		IdempotencyKey:   n.IdempotencyKey,
		BillingAccountID: n.BillingAccountID,
	}, nil
}
