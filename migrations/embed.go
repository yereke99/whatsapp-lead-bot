// Package migrations embeds the SQL schema files so the binary can migrate
// without shipping loose files alongside it.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
