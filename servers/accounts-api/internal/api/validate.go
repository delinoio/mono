// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/delinoio/mono/go/uuidv7"
)

// Field limits of the protos contract.
const (
	maxNameCodePoints = 100
	maxEmailBytes     = 254
	// maxUserIDBytes bounds a Logto user ID in a request. Logto user IDs are
	// short; the bound only rejects abuse.
	maxUserIDBytes = 256
)

// normalizeID parses a UUID v7 of either hex case and returns its lowercase
// text form.
func normalizeID(field, value string) (string, error) {
	id, err := uuidv7.Parse(value)
	if err != nil {
		return "", invalidArgument(field + " must be a UUID v7")
	}
	return id.String(), nil
}

// billingAccountName applies the name rules: white space at the start and
// the end is removed, and the rest is 1 to 100 code points without control
// characters.
func billingAccountName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameCodePoints {
		return "", invalidArgument("name must have 1 to 100 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", invalidArgument("name must not contain control characters")
		}
	}
	return name, nil
}

// invitationEmail applies the email rules: white space at the start and the
// end is removed, and the rest is a plain `local@domain` address of at most
// 254 bytes.
func invitationEmail(value string) (string, error) {
	addr := strings.TrimSpace(value)
	if addr == "" || len(addr) > maxEmailBytes {
		return "", invalidArgument("email must be an email address of at most 254 bytes")
	}
	parsed, err := mail.ParseAddress(addr)
	local, domain, ok := strings.Cut(addr, "@")
	if err != nil || parsed.Name != "" || parsed.Address != addr || !ok || local == "" || domain == "" ||
		strings.Contains(domain, "@") {
		return "", invalidArgument("email must be an email address such as name@example.com")
	}
	return addr, nil
}

// userIDField checks a Logto user ID in a request: 1 to 256 bytes of valid
// UTF-8 without a NUL character, which Postgres cannot store in text.
func userIDField(value string) (string, error) {
	if value == "" || len(value) > maxUserIDBytes || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return "", invalidArgument("user_id must be a Logto user ID")
	}
	return value, nil
}

// emailKey is the comparison key of an email address: ASCII letters in lower
// case, every other byte unchanged (the ASCII case folding of the contract).
func emailKey(addr string) string {
	var b strings.Builder
	b.Grow(len(addr))
	for i := 0; i < len(addr); i++ {
		c := addr[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}
