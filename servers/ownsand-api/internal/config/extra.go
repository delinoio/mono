// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Environment variables of the agent channel layer.
const (
	EnvNonceKey           = "OWNSAND_NONCE_KEY"
	EnvNonceKeyPrevious   = "OWNSAND_NONCE_KEY_PREVIOUS"
	EnvR2Endpoint         = "R2_ENDPOINT"
	EnvR2Bucket           = "R2_BUCKET"
	EnvR2AccessKeyID      = "R2_ACCESS_KEY_ID"
	EnvR2SecretAccessKey  = "R2_SECRET_ACCESS_KEY"
	EnvReleaseManifestURL = "OWNSAND_RELEASE_MANIFEST_URL"
	EnvSTUNAddr           = "STUN_ADDR"
	EnvSTUNURLs           = "OWNSAND_STUN_URLS"
)

// STUN defaults: loopback for local runs; the public URL for clients.
const (
	DefaultSTUNAddr = "127.0.0.1:3478"
	DefaultSTUNURLs = "stun:ownsand.api.delino.io:3478"
)

// MinNonceKeyBytes is the shortest accepted nonce key.
const MinNonceKeyBytes = 32

// R2 holds the R2 settings. Secret fields are never logged.
type R2 struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// Configured reports whether R2 is set up.
func (r R2) Configured() bool { return r.Endpoint != "" }

// Extra holds the configuration of the agent channel and data path layers.
type Extra struct {
	// Nonce keys of the device challenges. Empty disables CreateChallenge,
	// Authenticate and ConfirmRemoval (they answer unavailable).
	NonceKey         []byte
	NonceKeyPrevious []byte
	R2               R2
	// Release manifest location sent to agents.
	ReleaseManifestURL string
	// UDP listen address of the STUN server.
	STUNAddr string
	// STUN server URLs that callers and agents get.
	STUNURLs []string
}

func decodeKey(name, v string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil {
			if len(b) < MinNonceKeyBytes {
				return nil, fmt.Errorf("%s must decode to at least %d bytes", name, MinNonceKeyBytes)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("%s must be base64", name)
}

func loadExtra(lookup Lookup) (Extra, error) {
	var e Extra
	var err error
	if v := get(lookup, EnvNonceKey); v != "" {
		if e.NonceKey, err = decodeKey(EnvNonceKey, v); err != nil {
			return e, err
		}
	}
	if v := get(lookup, EnvNonceKeyPrevious); v != "" {
		if e.NonceKeyPrevious, err = decodeKey(EnvNonceKeyPrevious, v); err != nil {
			return e, err
		}
	}
	e.R2 = R2{
		Endpoint: get(lookup, EnvR2Endpoint), Bucket: get(lookup, EnvR2Bucket),
		AccessKeyID: get(lookup, EnvR2AccessKeyID), SecretAccessKey: get(lookup, EnvR2SecretAccessKey),
	}
	set := 0
	for _, v := range []string{e.R2.Endpoint, e.R2.Bucket, e.R2.AccessKeyID, e.R2.SecretAccessKey} {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != 4 {
		return e, fmt.Errorf("%s, %s, %s and %s must be set together", EnvR2Endpoint, EnvR2Bucket, EnvR2AccessKeyID, EnvR2SecretAccessKey)
	}
	if e.R2.Endpoint != "" {
		if err := validateHTTPSURL(EnvR2Endpoint, e.R2.Endpoint, true); err != nil {
			return e, err
		}
		if strings.ContainsAny(e.R2.Bucket, "/?#") {
			return e, fmt.Errorf("%s must be a bucket name", EnvR2Bucket)
		}
	}
	if v := get(lookup, EnvReleaseManifestURL); v != "" {
		if err := validateHTTPSURL(EnvReleaseManifestURL, v, true); err != nil {
			return e, err
		}
		e.ReleaseManifestURL = v
	}
	e.STUNAddr = DefaultSTUNAddr
	if v := get(lookup, EnvSTUNAddr); v != "" {
		if err := validateAddr(EnvSTUNAddr, v); err != nil {
			return e, err
		}
		e.STUNAddr = v
	}
	urls := get(lookup, EnvSTUNURLs)
	if urls == "" {
		urls = DefaultSTUNURLs
	}
	for _, u := range strings.Split(urls, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "stun:") {
			return e, fmt.Errorf("%s entries must be stun: URLs; got %q", EnvSTUNURLs, u)
		}
		e.STUNURLs = append(e.STUNURLs, u)
	}
	return e, nil
}
