// SPDX-License-Identifier: FSL-1.1-ALv2

package listenaddr

import (
	"strings"
	"testing"
)

func TestValidateAccepts(t *testing.T) {
	for _, value := range []string{
		":8080",
		"127.0.0.1:0",
		"[::1]:65535",
		"[fe80::1%eth0]:80",
		"localhost:80",
		"api.example.com.:443",
		"a-b.c:1",
	} {
		if err := Validate("ADDR", value); err != nil {
			t.Errorf("Validate(%q) = %v", value, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	long := strings.Repeat("a", 64)
	for value, want := range map[string]string{
		"127.0.0.1":       "STUN_ADDR must be a host:port address",
		":70000":          `STUN_ADDR port must be a number from 0 to 65535; got "70000"`,
		":http":           `STUN_ADDR port must be a number from 0 to 65535; got "http"`,
		"not a host:80":   `STUN_ADDR host must be a hostname or IP address; got "not a host"`,
		"-bad.example:80": "STUN_ADDR host must be a hostname or IP address",
		"bad..example:80": "STUN_ADDR host must be a hostname or IP address",
		long + ".com:80":  "STUN_ADDR host must be a hostname or IP address",
		"under_score:80":  "STUN_ADDR host must be a hostname or IP address",
	} {
		err := Validate("STUN_ADDR", value)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%q) = %v; want an error containing %q", value, err, want)
		}
	}
}
