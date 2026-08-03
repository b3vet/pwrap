package pwrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Document is a row from a JSONB collection.
type Document struct {
	ID        uuid.UUID      `json:"id"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Table is a handle to a named JSONB document collection.
// Collections share a single physical table (pwrap_documents) with a GIN index on `data`.
type Table struct {
	client     *Client
	collection string
}

// Table returns a handle for the named collection. Collections are logical and do not
// need to be created ahead of time — they come into existence on first Insert.
func (c *Client) Table(collection string) *Table {
	return &Table{client: c, collection: collection}
}

// InsertMany inserts a batch of documents in a single round-trip and returns the
// generated ids in the same order. Built on `unnest()` so a single SQL statement
// handles arbitrary batch sizes (no Postgres 65535-parameter ceiling).
//
// Empty input is a no-op (returns nil, nil).
func (t *Table) InsertMany(ctx context.Context, datas []any) ([]uuid.UUID, error) {
	if len(datas) == 0 {
		return nil, nil
	}
	payloads := make([]string, len(datas))
	for i, d := range datas {
		b, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("pwrap: table: marshal[%d]: %w", i, err)
		}
		payloads[i] = string(b)
	}
	ids := make([]uuid.UUID, 0, len(datas))
	err := t.client.run(ctx, func(ctx context.Context, q querier) error {
		rows, err := q.Query(ctx, `
			INSERT INTO pwrap_documents (collection, data)
			SELECT $1, d::jsonb
			  FROM unnest($2::text[]) WITH ORDINALITY AS u(d, ord)
			 ORDER BY ord
			RETURNING id
		`, t.collection, payloads)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// Insert stores a new document and returns its id. `data` is JSON-encoded, so any
// json.Marshal-able value works — structs, maps, etc.
func (t *Table) Insert(ctx context.Context, data any) (uuid.UUID, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = t.client.run(ctx, func(ctx context.Context, q querier) error {
		return q.QueryRow(ctx, `
			INSERT INTO pwrap_documents (collection, data)
			VALUES ($1, $2::jsonb)
			RETURNING id
		`, t.collection, payload).Scan(&id)
	})
	return id, err
}

// Get fetches a document by id.
func (t *Table) Get(ctx context.Context, id uuid.UUID) (Document, error) {
	var d Document
	var raw []byte
	err := t.client.run(ctx, func(ctx context.Context, q querier) error {
		err := q.QueryRow(ctx, `
			SELECT id, data, created_at, updated_at
			FROM pwrap_documents
			WHERE collection = $1 AND id = $2
		`, t.collection, id).Scan(&d.ID, &raw, &d.CreatedAt, &d.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return Document{}, err
	}
	if err := json.Unmarshal(raw, &d.Data); err != nil {
		return Document{}, err
	}
	return d, nil
}

// Find returns up to `limit` documents whose JSONB data contains `filter` (the `@>`
// containment operator — hits the GIN index). A nil or empty filter matches all.
// If limit <= 0, it defaults to 100.
func (t *Table) Find(ctx context.Context, filter map[string]any, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 100
	}
	if filter == nil {
		filter = map[string]any{}
	}
	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return nil, err
	}
	var out []Document
	err = t.client.run(ctx, func(ctx context.Context, q querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, data, created_at, updated_at
			FROM pwrap_documents
			WHERE collection = $1 AND data @> $2::jsonb
			ORDER BY created_at DESC
			LIMIT $3
		`, t.collection, filterJSON, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDoc(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Update merges `patch` into the existing document via the JSONB `||` concat (shallow merge).
// Nested keys are replaced, not deep-merged — call Get/Insert for complex updates.
func (t *Table) Update(ctx context.Context, id uuid.UUID, patch map[string]any) error {
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	var affected int64
	err = t.client.run(ctx, func(ctx context.Context, q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE pwrap_documents
			   SET data = data || $3::jsonb, updated_at = now()
			 WHERE collection = $1 AND id = $2
		`, t.collection, id, patchJSON)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a document. Returns ErrNotFound if the id is unknown to this collection.
func (t *Table) Delete(ctx context.Context, id uuid.UUID) error {
	var affected int64
	err := t.client.run(ctx, func(ctx context.Context, q querier) error {
		tag, err := q.Exec(ctx, `
			DELETE FROM pwrap_documents WHERE collection = $1 AND id = $2
		`, t.collection, id)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Count returns how many documents in the collection match `filter`.
func (t *Table) Count(ctx context.Context, filter map[string]any) (int64, error) {
	if filter == nil {
		filter = map[string]any{}
	}
	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return 0, err
	}
	var n int64
	err = t.client.run(ctx, func(ctx context.Context, q querier) error {
		return q.QueryRow(ctx, `
			SELECT COUNT(*) FROM pwrap_documents
			WHERE collection = $1 AND data @> $2::jsonb
		`, t.collection, filterJSON).Scan(&n)
	})
	return n, err
}

// --- scanning helpers ---------------------------------------------------------

func scanDoc(rows pgx.Rows) (Document, error) {
	var d Document
	var raw []byte
	if err := rows.Scan(&d.ID, &raw, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return Document{}, err
	}
	if err := json.Unmarshal(raw, &d.Data); err != nil {
		return Document{}, err
	}
	return d, nil
}
