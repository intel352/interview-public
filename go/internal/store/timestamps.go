package store

import "time"

// DatabaseTime is the canonical SQLite representation for all timestamp writes
// and comparisons. Fixed-width UTC text sorts chronologically without losing
// nanoseconds, including when a cursor has passed through JSON and back to Go.
// SQLite defaults use this same format; schema checks reject mixed formats.
func DatabaseTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
