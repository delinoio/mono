// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsTransientDBError(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("apply: %w", &pgconn.PgError{Code: code}) }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"serialization failure", pg("40001"), true},
		{"deadlock", pg("40P01"), true},
		{"connection failure", pg("08006"), true},
		{"admin shutdown", pg("57P01"), true},
		{"read only transaction", pg("25006"), true},
		{"too many connections", pg("53300"), true},
		{"unique violation", pg("23505"), false},
		{"check violation", pg("23514"), false},
		{"syntax error", pg("42601"), false},
		{"unexpected EOF", fmt.Errorf("commit: %w", io.ErrUnexpectedEOF), true},
		{"connect error", &pgconn.ConnectError{}, true},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", fmt.Errorf("apply: %w", context.DeadlineExceeded), false},
		{"other", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := isTransientDBError(tc.err); got != tc.want {
			t.Errorf("%s: isTransientDBError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
