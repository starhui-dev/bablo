// Package migrations embeds Bablo's ordered SQL-first PostgreSQL migrations.
package migrations

import "embed"

// Files contains only versioned SQL migrations. Applied migrations are never
// edited in place; a schema change always receives a new versioned file.
//
//go:embed *.sql
var Files embed.FS
