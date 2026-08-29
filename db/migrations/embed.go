// Package migrations exposes the schema migrations embedded in the Mesh binary.
package migrations

import "embed"

// Files contains every Goose SQL migration.
//
//go:embed *.sql
var Files embed.FS
