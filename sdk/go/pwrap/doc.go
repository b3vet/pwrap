// Package pwrap is the Go SDK for pwrap.
//
// The SDK bootstraps against a pwrapd control plane using a project API key,
// receives a short-lived Postgres DSN, and talks to the tenant schema directly
// via pgx. It exposes opinionated APIs for JSONB document collections (Table),
// pgvector search (Vector), and the River-backed job queue (Queue).
package pwrap
