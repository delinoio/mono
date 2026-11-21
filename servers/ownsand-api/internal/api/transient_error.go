// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// isTransientDBError reports whether a repeat of the same database
// transaction can succeed after err: a lost or refused connection, a
// serialization failure or deadlock, a lock that was not available, a
// server that shuts down, restarts or fails over to a read-only replica, or
// resource exhaustion. Constraint violations, data and syntax errors and
// every other server error are permanent: the same statements fail the same
// way. A cancelled or expired context is not transient here; the caller owns
// that deadline.
func isTransientDBError(err error) bool {
	// context.DeadlineExceeded implements net.Error, so exclude the context
	// errors before the network check.
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		if len(code) < 2 {
			return false
		}
		switch code[:2] {
		case "08", // connection exception
			"40", // transaction rollback: serialization failure, deadlock
			"53": // insufficient resources
			return true
		}
		switch code {
		case "55P03", // lock_not_available
			"25006", // read_only_sql_transaction (failover to a replica)
			"57P01", // admin_shutdown (also a terminated backend)
			"57P02", // crash_shutdown
			"57P03": // cannot_connect_now
			return true
		}
		return false
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	if pgconn.SafeToRetry(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
