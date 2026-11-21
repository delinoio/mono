// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

func TestCodeString(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("relay: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{connect.NewError(connect.CodeNotFound, errors.New("x")), "not_found"},
		{errors.New("plain"), "unknown"},
	} {
		if got := codeString(tc.err); got != tc.want {
			t.Errorf("codeString(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
