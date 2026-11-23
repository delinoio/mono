// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestServiceInternalErrorMapping checks how Service.internal maps an error
// to a Connect error: context errors keep their own status, an existing
// Connect error passes through unchanged, and anything else is logged and
// redacted as internal.
func TestServiceInternalErrorMapping(t *testing.T) {
	detail, err := connect.NewErrorDetail(wrapperspb.String("kept"))
	if err != nil {
		t.Fatal(err)
	}
	withDetail := connect.NewError(connect.CodeNotFound, errors.New("no such thing"))
	withDetail.AddDetail(detail)

	cases := []struct {
		name    string
		err     error
		want    connect.Code
		message string
		logged  bool
	}{
		{"raw deadline", context.DeadlineExceeded, connect.CodeDeadlineExceeded, "request deadline exceeded", false},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, "request deadline exceeded", false},
		{"raw canceled", context.Canceled, connect.CodeCanceled, "request canceled", false},
		{"wrapped canceled", fmt.Errorf("query: %w", context.Canceled), connect.CodeCanceled, "request canceled", false},
		{"connect error", withDetail, connect.CodeNotFound, "no such thing", false},
		{"plain error", errors.New("secret database detail"), connect.CodeInternal, "internal error", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Service{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			got := s.internal(context.Background(), "test op", tc.err)
			var ce *connect.Error
			if !errors.As(got, &ce) {
				t.Fatalf("internal(%v) = %T, want *connect.Error", tc.err, got)
			}
			if ce.Code() != tc.want {
				t.Fatalf("code = %v, want %v", ce.Code(), tc.want)
			}
			if ce.Message() != tc.message {
				t.Fatalf("message = %q, want %q", ce.Message(), tc.message)
			}
			if tc.err == error(withDetail) {
				if got != tc.err || len(ce.Details()) != 1 {
					t.Fatalf("connect error was not passed through unchanged: %v with %d details", got, len(ce.Details()))
				}
			}
			if strings.Contains(ce.Message(), "secret") {
				t.Fatalf("message %q leaks the cause", ce.Message())
			}
			if tc.logged != strings.Contains(logs.String(), "internal_error") {
				t.Fatalf("logged = %v, want %v; log: %s", !tc.logged, tc.logged, logs.String())
			}
		})
	}
}
