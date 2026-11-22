// SPDX-License-Identifier: FSL-1.1-ALv2

package weborigin

import (
	"net/url"
	"testing"
)

func TestCanonical(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://accounts.delino.io", "https://accounts.delino.io", true},
		{"HTTPS://Accounts.Delino.IO", "https://accounts.delino.io", true},
		{"https://accounts.delino.io:443", "https://accounts.delino.io", true},
		{"http://accounts.delino.io:80", "http://accounts.delino.io", true},
		{"https://accounts.delino.io:0443", "https://accounts.delino.io", true},
		{"https://accounts.delino.io:80", "https://accounts.delino.io:80", true},
		{"http://accounts.delino.io:443", "http://accounts.delino.io:443", true},
		{"http://127.0.0.1:13100", "http://127.0.0.1:13100", true},
		{"http://[::1]:80", "http://[::1]", true},
		{"http://[::1]:13100", "http://[::1]:13100", true},
		{"https://[2001:DB8::1]:443", "https://[2001:db8::1]", true},
		{"https://:443", "", false},
		{"https://host:99999", "", false},
		{"ftp://host", "", false},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", tc.in, err)
		}
		got, ok := Canonical(u)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Canonical(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := Canonical(nil); got != "" || ok {
		t.Errorf("Canonical(nil) = %q, %v", got, ok)
	}
}
