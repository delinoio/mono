// SPDX-License-Identifier: FSL-1.1-ALv2

package email

import (
	"html/template"
	"net/url"
	"strings"
	"time"

	ttemplate "text/template"
)

// InvitationPath is the web app page that accepts an invitation. The token
// goes into the URL fragment, so it never reaches the web server logs
// (docs/apps-delino-accounts-foundation.md).
const InvitationPath = "/billing/invitations/accept"

// InvitationLink returns `<webOrigin>/billing/invitations/accept#token=<token>`.
// The link carries the secret token; never log it.
func InvitationLink(webOrigin, token string) string {
	return strings.TrimRight(webOrigin, "/") + InvitationPath + "#token=" + url.QueryEscape(token)
}

// Invitation describes an invitation email.
type Invitation struct {
	InvitationID       string
	BillingAccountID   string
	BillingAccountName string
	// Role is the invited role in lower case: `admin` or `viewer`.
	Role       string
	To         string
	Link       string
	ExpireTime time.Time
}

type invitationData struct {
	Name    string
	Role    string
	Link    string
	Expires string
}

var invitationText = ttemplate.Must(ttemplate.New("text").Parse(`You are invited to the Delino billing account "{{.Name}}" as {{.Role}}.

Accept the invitation:
{{.Link}}

The invitation expires on {{.Expires}}. Sign in with the email address that received this message to accept it.

If you did not expect this invitation, you can ignore this email.
`))

var invitationHTML = template.Must(template.New("html").Parse(`<!doctype html>
<html lang="en">
<body style="font-family: sans-serif; line-height: 1.5;">
<p>You are invited to the Delino billing account <strong>{{.Name}}</strong> as {{.Role}}.</p>
<p><a href="{{.Link}}">Accept the invitation</a></p>
<p>The invitation expires on {{.Expires}}. Sign in with the email address that received this message to accept it.</p>
<p>If you did not expect this invitation, you can ignore this email.</p>
</body>
</html>
`))

// InvitationMessage renders the invitation email. html/template escapes the
// billing account name, which the inviter chose.
func InvitationMessage(inv Invitation) (Message, error) {
	data := invitationData{
		Name:    inv.BillingAccountName,
		Role:    inv.Role,
		Link:    inv.Link,
		Expires: inv.ExpireTime.UTC().Format("January 2, 2006 15:04 UTC"),
	}
	var text, html strings.Builder
	if err := invitationText.Execute(&text, data); err != nil {
		return Message{}, err
	}
	if err := invitationHTML.Execute(&html, data); err != nil {
		return Message{}, err
	}
	return Message{
		Kind:             KindInvitation,
		To:               inv.To,
		Subject:          "You are invited to a Delino billing account",
		Text:             text.String(),
		HTML:             html.String(),
		IdempotencyKey:   "invitation/" + inv.InvitationID,
		BillingAccountID: inv.BillingAccountID,
	}, nil
}
