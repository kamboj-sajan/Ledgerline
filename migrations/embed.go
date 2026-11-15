// Package migrations embeds the SQL schema migrations so the ledgerline binary
// can migrate a database without the .sql files being present on disk.
package migrations

import "embed"

// FS holds every migration, named NNNNN_description.sql and applied by goose
// in version order. Each file has an Up and a Down section; the integration
// tests apply all of them, roll all of them back, and apply them again.
//
//go:embed *.sql
var FS embed.FS
