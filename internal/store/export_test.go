package store

// SetGrepTimeoutMS overrides grepTimeoutMS for a test and returns a restorer.
func SetGrepTimeoutMS(ms int) func() {
	orig := grepTimeoutMS
	grepTimeoutMS = ms
	return func() { grepTimeoutMS = orig }
}

// SearchSQL lets store_test EXPLAIN what Search runs.
const SearchSQL = searchSQL
