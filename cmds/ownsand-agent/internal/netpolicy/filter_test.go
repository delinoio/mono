// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"testing"
)

func TestFilter(t *testing.T) {
	f := NewFilter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	public := netip.MustParseAddr("8.8.8.8")
	if f.Allows(public) {
		t.Fatal("a filter allows destinations before its first refresh")
	}
	f.SetRoutesForTest(func() ([]Route, []netip.Prefix, error) {
		return []Route{
				{Prefix: pfx("0.0.0.0/0"), Interface: "en0"},
				{Prefix: pfx("0.0.0.0/1"), Interface: "utun3"},
				{Prefix: pfx("128.0.0.0/1"), Interface: "utun3"},
				{Prefix: pfx("203.0.113.0/24"), Interface: "utun4"},
			}, []netip.Prefix{
				pfx("198.51.100.7/24"),
				pfx("2001:db8:1::5/64"),
			}, nil
	})
	if err := f.Refresh(); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"8.8.8.8":         true,
		"1.1.1.1":         true,
		"2606:4700::1":    true,
		"192.168.0.10":    false,
		"10.0.0.1":        false,
		"127.0.0.1":       false,
		"169.254.1.1":     false,
		"198.51.100.7":    false,
		"198.51.100.20":   false,
		"203.0.113.5":     false,
		"2001:db8:1::9":   false,
		"fe80::1":         false,
		"::ffff:10.0.0.1": false,
		"64:ff9b::a00:1":  false,
	} {
		if got := f.Allows(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", addr, got, want)
		}
	}
	// A failed refresh keeps the last state.
	f.SetRoutesForTest(func() ([]Route, []netip.Prefix, error) { return nil, nil, errors.New("boom") })
	if err := f.Refresh(); err == nil {
		t.Fatal("refresh error lost")
	}
	if !f.Allows(public) || f.Allows(netip.MustParseAddr("10.0.0.1")) {
		t.Fatal("a failed refresh changed the filter")
	}
}
