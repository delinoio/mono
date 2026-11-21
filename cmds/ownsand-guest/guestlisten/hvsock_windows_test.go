// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"strings"
	"testing"
)

func TestHvsockInvalidServiceID(t *testing.T) {
	if _, err := Listen("hvsock://not-a-guid"); err == nil || !strings.Contains(err.Error(), "invalid Hyper-V socket service ID") {
		t.Fatalf("hvsock with an invalid service ID: %v", err)
	}
}
