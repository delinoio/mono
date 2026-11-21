// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"net/netip"
	"testing"
)

func TestParseTunnelSpec(t *testing.T) {
	spec, ok, err := parseTunnelSpec("console=ttyS0 root=/dev/sda rw ownsand.tunnel=1025,172.30.0.6/30,172.30.0.5 panic=1")
	if !ok || err != nil {
		t.Fatalf("parse: %v %v", ok, err)
	}
	if spec.port != 1025 || spec.guest != netip.MustParsePrefix("172.30.0.6/30") || spec.gateway != netip.MustParseAddr("172.30.0.5") {
		t.Fatalf("spec %+v", spec)
	}
	if _, ok, err := parseTunnelSpec("console=hvc0 root=/dev/vda"); ok || err != nil {
		t.Fatalf("no parameter: %v %v", ok, err)
	}
	for _, bad := range []string{
		"ownsand.tunnel=1025",
		"ownsand.tunnel=0,172.30.0.6/30,172.30.0.5",
		"ownsand.tunnel=1025,fd00::2/64,172.30.0.5",
		"ownsand.tunnel=1025,172.30.0.6/30,nope",
	} {
		if _, ok, err := parseTunnelSpec(bad); !ok || err == nil {
			t.Errorf("%q: ok %v err %v", bad, ok, err)
		}
	}
}
