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

// Canonical returns the serialized origin of u: the scheme, the host name in
// lower case (an IPv6 address in brackets) and the port when it is not the
// default port of the scheme (80 for http, 443 for https). It reports false
// when u has no http or https scheme, no host name, or a port that is not a
// number from 0 to 65535. It does not check the path, query, fragment or user
// information of u; callers reject those before they call Canonical.
//
// A non-ASCII host name is only lower-cased: a browser sends it in its
// punycode form, so an operator must configure that form.
func Canonical(u *url.URL) (string, bool) {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
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
