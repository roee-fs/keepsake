package store

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsUnavailable reports whether err means the database is down or unreachable, as
// 2de90d2:src/keepsake/server/tools.py classifies psycopg.OperationalError.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}

	// Only an acquire's deadline is unavailability; context.DeadlineExceeded also satisfies net.Error below.
	var acquireErr *acquireError
	if errors.As(err, &acquireErr) && errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 is connection_exception; 57P0x is the server shutting the connection down.
		return strings.HasPrefix(pgErr.Code, "08") ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}

	if errors.Is(err, puddle.ErrClosedPool) {
		return true
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}
