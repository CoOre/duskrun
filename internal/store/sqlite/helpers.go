package sqlite

import (
	"database/sql"
	"time"
)

// unixToTime converts stored unix seconds to a time.Time in UTC.
func unixToTime(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// nullTime maps a nullable unix-seconds column to *time.Time (nil when NULL).
func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := unixToTime(n.Int64)
	return &t
}
