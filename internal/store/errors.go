package store

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsUnavailable reports whether err reflects the database being down or
// unreachable, rather than a statement the database understood and refused. A
// caller uses this to tell "try again shortly" apart from a real error, the way
// src/keepsake/server/tools.py classifies psycopg.OperationalError.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}

	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 is connection_exception; the 57P0x codes are the server
		// shutting the connection down (admin command, crash, or restart).
		return strings.HasPrefix(pgErr.Code, "08") ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, puddle.ErrClosedPool) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}
