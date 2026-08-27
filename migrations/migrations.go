// Package migrations embeds the SQL migration files for the request-log
// database so a compiled binary carries its own schema — no files to ship
// alongside it. go:embed is relative to the file's own directory, which is
// why this lives here in migrations/ rather than in internal/logstore/.
package migrations

import "embed"

// FS holds every migrations/*.sql file, applied in filename order by
// internal/logstore.Migrate.
//
//go:embed *.sql
var FS embed.FS
