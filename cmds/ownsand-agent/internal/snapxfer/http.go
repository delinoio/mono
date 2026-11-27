// SPDX-License-Identifier: FSL-1.1-ALv2

package snapxfer

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

// NewHTTPClient returns a client for transfers through signed storage URLs.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: checkRedirect}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	// Keep net/http's default redirect limit.
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	original := via[0]
	if req.URL.Opaque != "" || storageOrigin(req.URL) != storageOrigin(original.URL) {
		return errors.New("redirect leaves the signed storage origin")
	}
	if req.Method != original.Method {
		return errors.New("redirect changes the snapshot transfer method")
	}
	// net/http sets Referer from the previous URL, including its signed query.
	req.Header.Del("Referer")
	return nil
}

type origin struct {
	scheme string
	host   string
	port   string
}

func storageOrigin(u *url.URL) origin {
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	} else if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = strings.ToLower(ascii)
	}
	port := u.Port()
	switch {
	case port != "":
		port = strings.TrimLeft(port, "0")
	case u.Scheme == "https":
		port = "443"
	case u.Scheme == "http":
		port = "80"
	}
	return origin{scheme: u.Scheme, host: host, port: port}
}
