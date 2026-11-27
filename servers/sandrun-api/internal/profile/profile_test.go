// SPDX-License-Identifier: FSL-1.1-ALv2

package profile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
)

type failedInsertTx struct {
	pgx.Tx
	err error
}

func (tx failedInsertTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, tx.err
}

func TestInsertPreservesOtherErrors(t *testing.T) {
	svc := &api.Service{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"other foreign key", &pgconn.PgError{Code: "23503", ConstraintName: "runner_profiles_organization_id_fkey"}},
		{"other SQLSTATE", &pgconn.PgError{Code: "23514", ConstraintName: "runner_profiles_organization_id_agent_integration_id_fkey"}},
		{"unexpected error", errors.New("database unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := insert(t.Context(), failedInsertTx{err: tt.err}, &Profile{})
			if err != tt.err {
				t.Fatalf("insert error = %v, want %v", err, tt.err)
			}
			var rpcErr *connect.Error
			if err := svc.Internal(t.Context(), "create runner profile", err); !errors.As(err, &rpcErr) ||
				rpcErr.Code() != connect.CodeInternal || rpcErr.Message() != "internal error" {
				t.Fatalf("RPC error = %v, want internal: internal error", err)
			}
		})
	}
}
