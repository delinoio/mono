// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"net/http"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
)

func TestCodeString(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("relay: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{connect.NewError(connect.CodeNotFound, errors.New("x")), "not_found"},
		{errors.New("plain"), "unknown"},
	} {
		if got := codeString(tc.err); got != tc.want {
			t.Errorf("codeString(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestClientAddress(t *testing.T) {
	const peer = "198.51.100.7:4242"
	forward := func(values ...string) http.Header { return http.Header{"X-Forwarded-For": values} }
	for _, tc := range []struct {
		name   string
		peer   string
		header http.Header
		mode   config.ClientAddressMode
		want   string
	}{
		{"zero mode has no address", peer, forward("203.0.113.1"), "", ""},
		{"none mode has no address", peer, forward("203.0.113.1"), config.ClientAddressNone, ""},
		{"direct uses the peer host", peer, nil, config.ClientAddressDirect, "198.51.100.7"},
		{"direct ignores forwarded headers", peer, forward("203.0.113.1"), config.ClientAddressDirect, "198.51.100.7"},
		{"direct with a peer without a port", "198.51.100.7", nil, config.ClientAddressDirect, ""},
		{"direct with an IPv6 peer", "[2001:db8::1]:80", nil, config.ClientAddressDirect, "2001:db8::1"},
		{"verified uses the last entry", peer, forward("10.0.0.1, 203.0.113.1"), config.ClientAddressVerifiedProxy, "203.0.113.1"},
		{"verified uses the last header", peer, forward("10.0.0.1", "203.0.113.2"), config.ClientAddressVerifiedProxy, "203.0.113.2"},
		{"verified accepts IPv6", peer, forward("2001:db8::2"), config.ClientAddressVerifiedProxy, "2001:db8::2"},
		{"verified without a header has no address", peer, nil, config.ClientAddressVerifiedProxy, ""},
		{"verified with an invalid final value has no address", peer, forward("203.0.113.1, nope"), config.ClientAddressVerifiedProxy, ""},
		{"verified ignores an invalid earlier header", peer, forward("nope", "203.0.113.3"), config.ClientAddressVerifiedProxy, "203.0.113.3"},
		{"verified with an empty final entry has no address", peer, forward("203.0.113.1,"), config.ClientAddressVerifiedProxy, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientAddress(tc.peer, tc.header, tc.mode); got != tc.want {
				t.Fatalf("ClientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}
