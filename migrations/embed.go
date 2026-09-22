// Package migrations embeds the SQL migration files so they ship inside the
// single static binary (no external files to deploy).
package migrations

import "embed"

// FS holds the ordered *.sql migrations, applied lexically by name.
//
//go:embed *.sql
var FS embed.FS
