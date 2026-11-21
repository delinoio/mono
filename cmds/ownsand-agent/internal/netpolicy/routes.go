// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// ParseProcRoute parses /proc/net/route (IPv4, little-endian hex fields).
func ParseProcRoute(r io.Reader) ([]Route, error) {
	var out []Route
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		dst, err := strconv.ParseUint(f[1], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: route destination %q: %w", f[1], err)
		}
		mask, err := strconv.ParseUint(f[7], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: route mask %q: %w", f[7], err)
		}
		var d, m [4]byte
		binary.LittleEndian.PutUint32(d[:], uint32(dst))
		binary.LittleEndian.PutUint32(m[:], uint32(mask))
		bits := 0
		for _, b := range m {
			for i := 7; i >= 0; i-- {
				if b&(1<<i) != 0 {
					bits++
				}
			}
		}
		out = append(out, Route{Prefix: netip.PrefixFrom(netip.AddrFrom4(d), bits).Masked(), Interface: f[0]})
	}
	return out, sc.Err()
}

// ParseProcIPv6Route parses /proc/net/ipv6_route.
func ParseProcIPv6Route(r io.Reader) ([]Route, error) {
	var out []Route
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		raw, err := hex.DecodeString(f[0])
		if err != nil || len(raw) != 16 {
			return nil, fmt.Errorf("netpolicy: ipv6 route destination %q", f[0])
		}
		bits, err := strconv.ParseUint(f[1], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: ipv6 route length %q", f[1])
		}
		iface := f[9]
		if iface == "lo" {
			// Local and multicast routes of the loopback table: the host's
			// own addresses are dropped in the input chain.
			continue
		}
		out = append(out, Route{Prefix: netip.PrefixFrom(netip.AddrFrom16([16]byte(raw)), int(bits)).Masked(), Interface: iface})
	}
	return out, sc.Err()
}
