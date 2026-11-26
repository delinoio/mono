// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"strings"
	"testing"
)

func TestNewRejectsPortWithoutHostname(t *testing.T) {
	const want = "sandrun: invalid base URL: want an http or https origin"
	for _, base := range []string{
		"http://:18400",
		"https://:443",
		"http://:",
		"https://:0",
		"http://",
		"https://",
	} {
		t.Run(base, func(t *testing.T) {
			client, err := New(Options{BaseURL: base, APIKey: "sandrun_x"})
			if client != nil {
				t.Error("New returned a client for an origin without a hostname")
			}
			if err == nil || err.Error() != want {
				t.Fatalf("New returned error %v, want %q", err, want)
			}
			if strings.Contains(err.Error(), base) {
				t.Errorf("error echoes the origin: %v", err)
			}
		})
	}
}

func TestNewRejectsOutOfRangePorts(t *testing.T) {
	const want = "sandrun: invalid base URL: want a port between 0 and 65535"
	for _, base := range []string{
		"https://127.0.0.1:65536",
		"http://[::1]:65536",
		"https://127.0.0.1:999999999999999999999999",
	} {
		t.Run(base, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				opts Options
			}{
				{name: "APIKey", opts: Options{APIKey: "sandrun_x"}},
				{name: "TokenSource", opts: Options{TokenSource: func(context.Context) (string, error) {
					t.Error("New called the token source")
					return "", nil
				}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.opts.BaseURL = base
					client, err := New(tc.opts)
					if client != nil || err == nil || err.Error() != want {
						t.Fatalf("New returned client present = %t, error = %v; want no client and %q", client != nil, err, want)
					}
				})
			}
		})
	}
}

func TestNewAcceptsOriginsWithHostname(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:18400",
		"http://[::1]:18400",
		"http://[::1]:65535/",
		"http://[::1]",
		"https://127.0.0.1:0",
		"https://127.0.0.1:",
		"https://127.0.0.1",
		"https://example.test:443",
		"https://example.test",
		"http://localhost:8080/",
	} {
		t.Run(base, func(t *testing.T) {
			client, err := New(Options{BaseURL: base, APIKey: "sandrun_x"})
			if err != nil || client == nil {
				t.Fatalf("New rejected the origin: %v", err)
			}
			if got, want := client.BaseURL(), strings.TrimRight(base, "/"); got != want {
				t.Errorf("BaseURL() = %q, want %q", got, want)
			}
		})
	}

	client, err := New(Options{APIKey: "sandrun_x"})
	if err != nil || client == nil {
		t.Fatalf("New rejected the default origin: %v", err)
	}
	if got := client.BaseURL(); got != DefaultBaseURL {
		t.Errorf("BaseURL() = %q, want %q", got, DefaultBaseURL)
	}
}
