// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/tide-api/internal/api"
)

func TestInternalContextErrors(t *testing.T) {
	explicit := connect.NewError(connect.CodeUnavailable, fmt.Errorf("upstream timeout: %w", context.DeadlineExceeded))
	unexpected := errors.New("unexpected query failure")
	tests := []struct {
		name    string
		err     error
		code    connect.Code
		message string
	}{
		{"deadline", context.DeadlineExceeded, connect.CodeDeadlineExceeded, "request deadline exceeded"},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, "request deadline exceeded"},
		{"canceled", context.Canceled, connect.CodeCanceled, "request canceled"},
		{"wrapped cancellation", fmt.Errorf("query: %w", context.Canceled), connect.CodeCanceled, "request canceled"},
		{"explicit Connect error", explicit, connect.CodeUnavailable, "upstream timeout: context deadline exceeded"},
		{"unexpected error", unexpected, connect.CodeInternal, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &api.Service{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			got := service.Internal(context.Background(), "get ownsand link", tt.err)
			var connectErr *connect.Error
			if !errors.As(got, &connectErr) {
				t.Fatalf("Internal returned %T, want *connect.Error", got)
			}
			if connectErr.Code() != tt.code || connectErr.Message() != tt.message {
				t.Errorf("Internal returned %v with %q, want %v with %q", connectErr.Code(), connectErr.Message(), tt.code, tt.message)
			}
			if tt.err == explicit && got != explicit {
				t.Errorf("Internal returned %v, want original Connect error", got)
			}
			if tt.err == unexpected {
				log := logs.String()
				if !strings.Contains(log, `"msg":"internal_error"`) ||
					!strings.Contains(log, `"op":"get ownsand link"`) ||
					!strings.Contains(log, `"error":"unexpected query failure"`) {
					t.Errorf("Internal did not log the operation and cause: %s", log)
				}
			} else if logs.Len() != 0 {
				t.Errorf("Internal logged an expected error: %s", &logs)
			}
		})
	}
}
