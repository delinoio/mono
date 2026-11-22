// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Page sizes of every list RPC.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// pageSize applies the list rules: 0 selects the default, values above the
// maximum are lowered and a negative value is invalid.
func pageSize(requested int32) (int, error) {
	switch {
	case requested < 0:
		return 0, invalidArgument("page_size must not be negative")
	case requested == 0:
		return defaultPageSize, nil
	case requested > maxPageSize:
		return maxPageSize, nil
	default:
		return int(requested), nil
	}
}

// pageToken is the decoded form of an opaque page token. It binds the
// position to the RPC and to its scope (the caller or the billing account),
// so a token of another request is rejected. Tokens are not signed: a
// forged token can only select another position in a result that the
// caller may read anyway.
type pageToken struct {
	// RPC names the list RPC.
	RPC string `json:"r"`
	// Scope is the caller's user ID or the billing account ID.
	Scope string `json:"s"`
	// After is the sort key of the last returned item.
	After string `json:"a"`
	// AfterTime is the time part of a sort key, in Unix microseconds.
	AfterTime int64 `json:"t,omitempty"`
}

func encodePageToken(tok pageToken) string {
	body, _ := json.Marshal(tok)
	return base64.RawURLEncoding.EncodeToString(body)
}

// decodePageToken returns the token for the RPC and scope, the zero token
// for an empty string, and invalid_argument for any other value. Because
// tokens are not signed, the position is checked too: a negative time
// (the server issues none) and a NUL character in the sort key (which
// Postgres cannot store in text) are invalid, so a forged token never
// reaches a query that fails with an internal error.
func decodePageToken(raw, rpc, scope string) (pageToken, error) {
	if raw == "" {
		return pageToken{}, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	var tok pageToken
	if err != nil || json.Unmarshal(body, &tok) != nil || tok.RPC != rpc || tok.Scope != scope || tok.After == "" ||
		tok.AfterTime < 0 || strings.ContainsRune(tok.After, 0) {
		return pageToken{}, invalidArgument("page_token is not valid for this request")
	}
	return tok, nil
}
