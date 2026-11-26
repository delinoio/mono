// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
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

func TestNewAcceptsOriginsWithHostname(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:18400",
		"http://[::1]:18400",
		"http://[::1]",
		"https://example.test:443",
		"https://example.test",
		"http://localhost:8080/",
	} {
		t.Run(base, func(t *testing.T) {
			client, err := New(Options{BaseURL: base, APIKey: "sandrun_x"})
			if err != nil || client == nil {
				t.Fatalf("New rejected the origin: %v", err)
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
