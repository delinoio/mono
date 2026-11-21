// SPDX-License-Identifier: FSL-1.1-ALv2

// Package listenaddr validates the host:port listen addresses that servers
// read from their configuration (for example ADDR). It checks only the
// syntax and does not resolve host names, so a host name that does not
// resolve still fails when the server listens.
package listenaddr

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

const (
	maxHostnameLength = 253
	maxLabelLength    = 63
)

// Validate checks that value is a host:port address. name is the
// configuration variable that holds the value; error messages start with
// it. The port must be a number from 0 to 65535. The host must be empty
// (all interfaces), an IP address (IPv6 in brackets, with an optional
// zone) or an RFC 1123 host name.
func Validate(name, value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address: %w", name, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%s port must be a number from 0 to 65535; got %q", name, port)
	}
	if !validHost(host) {
		return fmt.Errorf("%s host must be a hostname or IP address; got %q", name, host)
	}
	return nil
}

func validHost(host string) bool {
	if host == "" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return isHostname(host)
}

// isHostname reports whether name is an RFC 1123 hostname: dot-separated
// labels of 1 to 63 ASCII letters, digits or hyphens that neither start nor
// end with a hyphen, at most 253 characters, with an optional trailing dot.
func isHostname(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > maxHostnameLength {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > maxLabelLength {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
			if !isAlnum && c != '-' {
				return false
			}
		}
	}
	return true
}
