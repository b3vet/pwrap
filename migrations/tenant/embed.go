// Package tenant exposes the baseline tenant migration as an embedded FS, so pwrapd
// ships with it without depending on a migration directory on disk.
package tenant

import "embed"

//go:embed *.sql
var FS embed.FS
