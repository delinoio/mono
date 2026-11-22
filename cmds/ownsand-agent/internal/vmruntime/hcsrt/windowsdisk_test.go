// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

func TestCheckWindowsDiskSize(t *testing.T) {
	for _, tc := range []struct {
		name              string
		requested, actual int64
		wantErr           string
	}{
		{name: "equal", requested: 32 * gib, actual: 32 * gib},
		{name: "grow", requested: 40 * gib, actual: 32 * gib},
		{name: "rounds up to the alignment", requested: 32*gib - 1, actual: 32 * gib},
		{name: "one alignment unit below", requested: 32*gib - vhdAlign, actual: 32 * gib,
			wantErr: "needs at least 32 GiB, not the requested 31 GiB"},
		{name: "undersized", requested: 8 * gib, actual: 32 * gib,
			wantErr: "needs at least 32 GiB, not the requested 8 GiB"},
		{name: "required size rounds up", requested: 8 * gib, actual: 12*gib + vhdAlign,
			wantErr: "needs at least 13 GiB, not the requested 8 GiB"},
		{name: "no request", requested: 0, actual: 32 * gib,
			wantErr: "needs at least 32 GiB, not the requested 0 GiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWindowsDiskSize(tc.requested, tc.actual)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, vmruntime.ErrUnsupported) {
				t.Fatalf("error %v does not wrap ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q lacks %q", err, tc.wantErr)
			}
		})
	}
}
