// Package database publishes the SQL migrations so the binary can apply
// them at startup. The live database never lives in the repository: it is
// created under the data directory (/data by default).
package database

import "embed"

// Migrations holds the goose migrations, applied in order at startup.
//
//go:embed migrations/*.sql
var Migrations embed.FS
