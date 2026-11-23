// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"strings"
	"testing"
)

func TestNewRejectsPortWithoutHostname(t *testing.T) {
	for _, bad := range []string{"http://:18300", "https://:443", "http://:", "https://:0"} {
		c, err := New(Options{BaseURL: bad, APIKey: "osk_x", LookupEnv: envOf(nil)})
		if err == nil {
			t.Fatalf("base URL %q accepted", bad)
		}
		if c != nil {
			t.Fatalf("base URL %q returned a client with an error", bad)
		}
		if want := "ownsand: invalid base URL: want an http or https origin"; err.Error() != want {
			t.Fatalf("base URL %q: error %q, want %q", bad, err, want)
		}
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("error echoes the base URL: %v", err)
		}
	}
}

func TestNewAcceptsOriginsWithHostname(t *testing.T) {
	for _, good := range []string{
		"http://127.0.0.1:18300",
		"http://[::1]:18300",
		"http://[::1]",
		"https://example.test:443",
		"https://example.test",
		"http://localhost:8080/",
	} {
		c, err := New(Options{BaseURL: good, APIKey: "osk_x", LookupEnv: envOf(nil)})
		if err != nil || c == nil {
			t.Fatalf("base URL %q rejected: %v", good, err)
		}
	}
	c, err := New(Options{APIKey: "osk_x", LookupEnv: envOf(nil)})
	if err != nil || c.BaseURL() != DefaultBaseURL {
		t.Fatalf("default origin: %v %v", c, err)
	}
}
