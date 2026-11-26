// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type beforeSendError struct{}

func (beforeSendError) Error() string     { return "query was never sent" }
func (beforeSendError) SafeToRetry() bool { return true }

func TestCreateDatabaseCleanup(t *testing.T) {
	const name = "ownsand_test_0011223344556677"
	for _, tc := range []struct {
		label       string
		err         error
		wantCleanup int
	}{
		{label: "success", wantCleanup: 1},
		{label: "lost reply", err: io.EOF, wantCleanup: 1},
		{label: "duplicate database", err: &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "42P04"}},
		{label: "permission denied", err: &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "42501"}},
		{label: "before send", err: beforeSendError{}},
		{label: "session terminated", err: &pgconn.PgError{SeverityUnlocalized: "FATAL", Code: "57P01"}, wantCleanup: 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			registrations := 0
			err := createDatabase(name, func(sql string) error {
				if sql != "CREATE DATABASE "+name {
					t.Fatalf("create statement = %q", sql)
				}
				return tc.err
			}, func() { registrations++ })
			if !errors.Is(err, tc.err) {
				t.Errorf("create error = %v, want %v", err, tc.err)
			}
			if registrations != tc.wantCleanup {
				t.Errorf("cleanup registrations = %d, want %d", registrations, tc.wantCleanup)
			}
		})
	}
}
