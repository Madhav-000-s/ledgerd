// Package migrations embeds the schema so that both binaries and the integration suite
// carry it with them.
//
// Embedding rather than shipping a directory means a container image cannot be built
// without its migrations, and the test suite cannot drift from what production runs.
package migrations

import "embed"

// FS holds every migration, in goose's plain-SQL format.
//
//go:embed *.sql
var FS embed.FS
