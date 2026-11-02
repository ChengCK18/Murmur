// Package migrations embeds the SQL schema, applied in file-name order by
// internal/db.Migrate.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
