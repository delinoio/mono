// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

type sessionQuery struct {
	db.Querier
	rows pgx.Rows
}

func (q sessionQuery) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return q.rows, nil
}

type sessionTx struct {
	pgx.Tx
	rows pgx.Rows
}

func (tx sessionTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return tx.rows, nil
}

type sessionRows struct {
	pgx.Rows
	scan      func([]any) error
	remaining int
	iteration error
	closed    bool
}

func (r *sessionRows) Next() bool {
	if r.remaining == 0 {
		r.Close()
		return false
	}
	r.remaining--
	return true
}

func (r *sessionRows) Scan(dest ...any) error { return r.scan(dest) }

func (r *sessionRows) Close() { r.closed = true }

func (r *sessionRows) Err() error {
	if !r.closed {
		panic("rows must close before checking Err")
	}
	return r.iteration
}

var errSessionRows = errors.New("dev session row iteration failed")

func TestDevSessionCompleteRows(t *testing.T) {
	h := &Handler{}
	t.Run("runner iteration fails", func(t *testing.T) {
		q := sessionQuery{rows: &sessionRows{iteration: errSessionRows}}
		sessions, err := h.complete(t.Context(), q, []*work.Request{{RunnerID: "runner"}})
		if sessions != nil || !errors.Is(err, errSessionRows) {
			t.Fatalf("complete = %v, %v", sessions, err)
		}
	})
	for _, iterationErr := range []error{nil, errSessionRows} {
		name := "profile iteration succeeds"
		if iterationErr != nil {
			name = "profile iteration fails after a row"
		}
		t.Run(name, func(t *testing.T) {
			q := sessionQuery{rows: &sessionRows{remaining: 1, iteration: iterationErr, scan: func(dest []any) error {
				*dest[0].(*string) = "profile"
				*dest[1].(*string) = "linux-dev"
				*dest[2].(*string) = "/home/dev/work"
				*dest[3].(*int16) = int16(sandrunv1.GuestOs_GUEST_OS_LINUX)
				return nil
			}}}
			sessions, err := h.complete(t.Context(), q, []*work.Request{{ProfileID: "profile"}})
			if iterationErr != nil {
				if sessions != nil || !errors.Is(err, iterationErr) {
					t.Fatalf("complete = %v, %v", sessions, err)
				}
				return
			}
			if err != nil || len(sessions) != 1 || sessions[0].profileName != "linux-dev" || sessions[0].workspace != "/home/dev/work" || sessions[0].guestOS != sandrunv1.GuestOs_GUEST_OS_LINUX {
				t.Fatalf("complete = %v, %v", sessions, err)
			}
		})
	}
}

func TestDevSessionListRequestRowError(t *testing.T) {
	q := sessionQuery{rows: &sessionRows{iteration: errSessionRows}}
	requests, err := listRequests(t.Context(), q, "org", api.AfterKey(""), "", "", nil, 10)
	if requests != nil || !errors.Is(err, errSessionRows) {
		t.Fatalf("listRequests = %v, %v", requests, err)
	}
}

func TestDevSessionMembershipEndTxRowError(t *testing.T) {
	// Include an open session. An incomplete read must return before the
	// hook uses the work engine to end any session.
	tx := sessionTx{rows: &sessionRows{remaining: 1, iteration: errSessionRows, scan: func(dest []any) error {
		*dest[0].(*string) = "session"
		return nil
	}}}
	if err := (&Handler{}).membershipEndTx(t.Context(), tx, api.MembershipEnd{}); !errors.Is(err, errSessionRows) {
		t.Fatalf("membershipEndTx = %v", err)
	}
}

func TestDevSessionMemberCleanupRunnerRows(t *testing.T) {
	scanErr := errors.New("runner ID scan failed")
	for _, tc := range []struct {
		name      string
		thirdScan bool
		iteration error
		wantErr   error
	}{
		{name: "success"},
		{name: "iteration fails", iteration: errSessionRows, wantErr: errSessionRows},
		{name: "scan fails", thirdScan: true, wantErr: scanErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantIDs := []string{"runner-1", "runner-2"}
			remaining := len(wantIDs)
			if tc.thirdScan {
				remaining++
			}
			scanned := 0
			rows := &sessionRows{remaining: remaining, iteration: tc.iteration, scan: func(dest []any) error {
				if scanned == len(wantIDs) {
					return scanErr
				}
				*dest[0].(*string) = wantIDs[scanned]
				scanned++
				return nil
			}}
			got, err := memberCleanupRunnerIDs(t.Context(), sessionQuery{rows: rows}, api.MembershipEnd{})
			if !slices.Equal(got, wantIDs) || !errors.Is(err, tc.wantErr) || !rows.closed {
				t.Fatalf("memberCleanupRunnerIDs = %v, %v (closed: %t)", got, err, rows.closed)
			}
		})
	}
}
