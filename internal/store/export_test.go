package store

import "time"

// SetAcquireTimeout overrides acquireTimeout for a test and returns a restorer.
// Exported only to this package's tests, per Go's export_test.go convention: the
// external isolation_test.go package needs it and cannot see the unexported var.
func SetAcquireTimeout(d time.Duration) func() {
	orig := acquireTimeout
	acquireTimeout = d
	return func() { acquireTimeout = orig }
}
