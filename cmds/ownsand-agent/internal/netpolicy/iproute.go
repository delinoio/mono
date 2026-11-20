package netpolicy

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

// ParseAddresses parses the output of `ip -json address show`.
func ParseAddresses(data []byte) ([]Address, error) {
	var links []struct {
		IfName   string `json:"ifname"`
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, fmt.Errorf("parse ip address output: %w", err)
	}
	var out []Address
	for _, l := range links {
		for _, a := range l.AddrInfo {
			addr, err := netip.ParseAddr(a.Local)
			if err != nil {
				continue
			}
			p := netip.PrefixFrom(addr.WithZone(""), a.PrefixLen)
			if !p.IsValid() {
				continue
			}
			out = append(out, Address{Interface: l.IfName, Prefix: p})
		}
	}
	return out, nil
}

// ParseRoutes parses the output of `ip -json route show table all` (IPv4)
// or `ip -6 -json route show table all`. It keeps unicast routes, including
// the default route, and drops local, broadcast, multicast, unreachable,
// blackhole, prohibit and throw routes, which the host does not use to reach
// another network.
func ParseRoutes(data []byte, v6 bool) ([]Route, error) {
	var routes []struct {
		Dst  string `json:"dst"`
		Dev  string `json:"dev"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("parse ip route output: %w", err)
	}
	var out []Route
	for _, r := range routes {
		if r.Type != "" && r.Type != "unicast" {
			continue
		}
		var p netip.Prefix
		switch r.Dst {
		case "default":
			if v6 {
				p = netip.MustParsePrefix("::/0")
			} else {
				p = netip.MustParsePrefix("0.0.0.0/0")
			}
		default:
			var err error
			p, err = netip.ParsePrefix(r.Dst)
			if err != nil {
				a, aerr := netip.ParseAddr(r.Dst)
				if aerr != nil {
					continue
				}
				p = netip.PrefixFrom(a.WithZone(""), a.BitLen())
			}
		}
		out = append(out, Route{Prefix: p, Interface: r.Dev})
	}
	return out, nil
}
