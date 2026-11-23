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
		// IPv4 spellings that a browser normalizes to dotted decimal.
		{"http://127.1:3000", "http://127.0.0.1:3000", true},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000", true},
		{"http://127.1:80", "http://127.0.0.1", true},
		{"http://0x7f.1:3000", "http://127.0.0.1:3000", true},
		{"http://0X7F.0.0.1", "http://127.0.0.1", true},
		{"http://2130706433:3000", "http://127.0.0.1:3000", true},
		{"http://0x7f000001", "http://127.0.0.1", true},
		{"http://017700000001", "http://127.0.0.1", true},
		{"http://0300.0250.0.1", "http://192.168.0.1", true},
		{"http://192.168.1", "http://192.168.0.1", true},
		{"http://10.65537", "http://10.1.0.1", true},
		{"https://127.0.0.1.", "https://127.0.0.1", true},
		{"https://127.1.:8443", "https://127.0.0.1:8443", true},
		{"https://0:443", "https://0.0.0.0", true},
		{"https://0x:443", "https://0.0.0.0", true},
		{"https://255.255.255.255", "https://255.255.255.255", true},
		{"https://4294967295", "https://255.255.255.255", true},
		{"https://0.0.0.0", "https://0.0.0.0", true},
		// A host whose last label is a number and that is not an IPv4 address is rejected.
		{"https://256.1.1.1", "", false},
		{"https://1.256.1.1", "", false},
		{"https://1.2.3.4.5", "", false},
		{"https://4294967296", "", false},
		{"https://1.16777216", "", false},
		{"https://10.0.65536", "", false},
		{"https://999999999999999999999", "", false},
		{"https://08.1", "", false},
		{"https://example.1", "", false},
		{"https://example.0x10", "", false},
		// A name that merely contains digits stays a name.
		{"https://1.example.com", "https://1.example.com", true},
		{"https://127.example", "https://127.example", true},
		{"https://127.0.0.1.example", "https://127.0.0.1.example", true},
		{"https://0x7f.example.com:8443", "https://0x7f.example.com:8443", true},
		{"https://a1.b2", "https://a1.b2", true},
		{"https://0x1g", "https://0x1g", true},
		{"https://127.0.0.1..", "https://127.0.0.1..", true},
		{"https://example.com.", "https://example.com.", true},
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
