// Package controlplane embeds pwrapd's own DDL so the binary can self-bootstrap.
package controlplane

import _ "embed"

// UpSQL is the entire control-plane DDL applied by pwrapd on startup.
// Every statement is idempotent (IF NOT EXISTS) so re-applying is safe.
//
//go:embed 0001_init.up.sql
var UpSQL string
