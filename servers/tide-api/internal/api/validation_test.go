// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
)

func TestDefinedEnum(t *testing.T) {
	cases := []struct {
		number int32
		valid  bool
	}{
		{0, true},
		{1, true},
		{2, true},
		{3, true},
		{4, false},
		{99, false},
		{-1, false},
		{65536, false},
		{65537, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.number), func(t *testing.T) {
			err := api.DefinedEnum("kind", "WorkloadKind", tidev1.WorkloadKind(tc.number), tidev1.WorkloadKind_name)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeInvalidArgument ||
				connectErr.Message() != "invalid kind: must be a defined WorkloadKind value" {
				t.Fatalf("expected invalid_argument for kind, got %v", err)
			}
		})
	}
}
