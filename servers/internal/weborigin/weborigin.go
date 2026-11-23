// SPDX-License-Identifier: FSL-1.1-ALv2

// Package weborigin turns a configured browser origin into the form that a
// browser sends in the Origin header, so a server can compare the header with
// its configured origins by exact string match. It checks only the syntax and
// does not resolve host names.
package weborigin

import (
	"net/url"
	"strconv"
	"strings"
)

// Canonical returns the serialized origin of u: the scheme, the host in the
// form that a browser serializes it (a name in lower case, an IPv4 address in
// dotted decimal, an IPv6 address in brackets) and the port when it is not the
// default port of the scheme (80 for http, 443 for https). It reports false
// when u has no http or https scheme, no host name, a host that a browser
// rejects (see below), or a port that is not a number from 0 to 65535. It does
// not check the path, query, fragment or user information of u; callers reject
// those before they call Canonical.
//
// A host whose last label is a number follows the WHATWG URL host parser, so an
// IPv4 spelling such as 127.1, 0x7f.1, 2130706433 or 0177.0.0.1 is stored as
// 127.0.0.1, as a browser sends it. Such a host that is not an IPv4 address (for
// example 1.2.3.4.5, 256.1.1.1 or example.1) is rejected, as browsers reject it.
// A non-ASCII host name is only lower-cased: a browser sends it in its
// punycode form, so an operator must configure that form.
func Canonical(u *url.URL) (string, bool) {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	} else if endsInNumber(host) {
		addr, ok := parseIPv4(host)
		if !ok {
			return "", false
		}
		host = addr
	}
	origin := u.Scheme + "://" + host
	if raw := u.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 0 || port > 65535 {
			return "", false
		}
		if (u.Scheme == "https" && port == 443) || (u.Scheme == "http" && port == 80) {
			return origin, true
		}
		origin += ":" + strconv.Itoa(port)
	}
	return origin, true
}

// endsInNumber implements the "ends in a number" check of the WHATWG URL
// standard: it reports whether the last label of host (ignoring one trailing
// dot) is a number, in which case the host is parsed as an IPv4 address and not
// as a domain name.
func endsInNumber(host string) bool {
	labels := strings.Split(host, ".")
	if labels[len(labels)-1] == "" {
		if len(labels) == 1 {
			return false
		}
		labels = labels[:len(labels)-1]
	}
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	if isDecimal(last) {
		return true
	}
	_, ok := parseIPv4Number(last)
	return ok
}

func isDecimal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// parseIPv4 implements the WHATWG IPv4 parser and returns the address in dotted
// decimal. It accepts one to four parts, each in decimal, hexadecimal (0x
// prefix) or octal (leading 0), where the last part fills the remaining bytes,
// and one trailing dot.
func parseIPv4(host string) (string, bool) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return "", false
	}
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		n, ok := parseIPv4Number(part)
		if !ok {
			return "", false
		}
		numbers[i] = n
	}
	for _, n := range numbers[:len(numbers)-1] {
		if n > 255 {
			return "", false
		}
	}
	last := numbers[len(numbers)-1]
	// The last part fills the remaining 4-(len-1) bytes.
	if last >= uint64(1)<<(8*(5-len(numbers))) {
		return "", false
	}
	ipv4 := last
	for i, n := range numbers[:len(numbers)-1] {
		ipv4 += n << (8 * (3 - i))
	}
	return strconv.Itoa(int(ipv4>>24)) + "." + strconv.Itoa(int(ipv4>>16&255)) + "." +
		strconv.Itoa(int(ipv4>>8&255)) + "." + strconv.Itoa(int(ipv4&255)), true
}

// parseIPv4Number parses one IPv4 part in decimal, hexadecimal (0x or 0X
// prefix) or octal (leading 0). A value above what a part can hold saturates
// at 1<<40, so a caller rejects it by range and not by syntax.
func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	radix := uint64(10)
	switch {
	case len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X"):
		s, radix = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, radix = s[1:], 8
	}
	const saturated = uint64(1) << 40
	var n uint64
	for i := 0; i < len(s); i++ {
		var digit uint64
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digit = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			digit = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = uint64(c-'A') + 10
		default:
			return 0, false
		}
		if digit >= radix {
			return 0, false
		}
		if n < saturated {
			n = n*radix + digit
		}
	}
	if n > saturated {
		n = saturated
	}
	return n, true
}
