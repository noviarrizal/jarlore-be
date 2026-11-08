// Package dbfiles embeds the SQL migrations so the migrate binary is self-contained.
package dbfiles

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
