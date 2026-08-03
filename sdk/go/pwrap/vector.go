package pwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// VectorDim is the fixed embedding dimension for MVP (OpenAI text-embedding-3-small / -ada-002 default).
// Post-MVP this becomes per-collection, stored in a registry table.
const VectorDim = 1536

// Match is a single nearest-neighbour result.
// Distance is cosine distance (1 - cosine similarity); lower is closer.
type Match struct {
	DocID     string         `json:"doc_id"`
	Metadata  map[string]any `json:"metadata"`
	Distance  float32        `json:"distance"`
}

// Vector is a handle over a named pgvector collection.
type Vector struct {
	client     *Client
	collection string
}

func (c *Client) Vector(collection string) *Vector {
	return &Vector{client: c, collection: collection}
}

// VectorRecord is one row for UpsertMany — same shape as the args to Upsert, batched.
type VectorRecord struct {
	DocID     string
	Embedding []float32
	Metadata  map[string]any
}

// UpsertMany writes a batch of (collection, doc_id) rows in a single round-trip.
// Built on `unnest()` so batch size is unbounded by Postgres's parameter limit.
// All records' embeddings must have length VectorDim; the call returns an error on
// the first mismatch without touching the DB. ON CONFLICT semantics are identical
// to Upsert — repeated calls with the same doc_id replace the previous embedding.
func (v *Vector) UpsertMany(ctx context.Context, records []VectorRecord) error {
	if len(records) == 0 {
		return nil
	}
	docIDs := make([]string, len(records))
	embs := make([]string, len(records))
	metas := make([]string, len(records))
	for i, r := range records {
		if len(r.Embedding) != VectorDim {
			return fmt.Errorf("pwrap: vector: record %d: expected dim %d, got %d", i, VectorDim, len(r.Embedding))
		}
		docIDs[i] = r.DocID
		embs[i] = formatVector(r.Embedding)
		m, err := json.Marshal(orEmptyMap(r.Metadata))
		if err != nil {
			return err
		}
		metas[i] = string(m)
	}
	return v.client.run(ctx, func(ctx context.Context, q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
			SELECT $1, doc_id, emb::vector, meta::jsonb
			  FROM unnest($2::text[], $3::text[], $4::text[]) AS t(doc_id, emb, meta)
			    ON CONFLICT (collection, doc_id) DO UPDATE
			       SET embedding  = EXCLUDED.embedding,
			           metadata   = EXCLUDED.metadata,
			           created_at = now()
		`, v.collection, docIDs, embs, metas)
		return err
	})
}

// Upsert writes (collection, doc_id) -> (embedding, metadata). Replaces any prior row.
func (v *Vector) Upsert(ctx context.Context, docID string, embedding []float32, metadata map[string]any) error {
	if len(embedding) != VectorDim {
		return fmt.Errorf("pwrap: vector: expected dim %d, got %d", VectorDim, len(embedding))
	}
	metaJSON, err := json.Marshal(orEmptyMap(metadata))
	if err != nil {
		return err
	}
	_, err = v.client.Pool().Exec(ctx, `
		INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
		VALUES ($1, $2, $3::vector, $4::jsonb)
		ON CONFLICT (collection, doc_id) DO UPDATE
		   SET embedding = EXCLUDED.embedding,
		       metadata  = EXCLUDED.metadata,
		       created_at = now()
	`, v.collection, docID, formatVector(embedding), metaJSON)
	if err != nil {
		return fmt.Errorf("pwrap: vector: upsert: %w", err)
	}
	return nil
}

// Delete removes a single (collection, doc_id) row.
func (v *Vector) Delete(ctx context.Context, docID string) error {
	tag, err := v.client.Pool().Exec(ctx, `
		DELETE FROM pwrap_embeddings WHERE collection = $1 AND doc_id = $2
	`, v.collection, docID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Search returns the top-k nearest neighbours (cosine distance) to the query vector.
// k <= 0 defaults to 10.
func (v *Vector) Search(ctx context.Context, query []float32, k int) ([]Match, error) {
	if len(query) != VectorDim {
		return nil, fmt.Errorf("pwrap: vector: expected dim %d, got %d", VectorDim, len(query))
	}
	if k <= 0 {
		k = 10
	}
	rows, err := v.client.Pool().Query(ctx, `
		SELECT doc_id, metadata, embedding <=> $1::vector AS distance
		FROM pwrap_embeddings
		WHERE collection = $2
		ORDER BY embedding <=> $1::vector
		LIMIT $3
	`, formatVector(query), v.collection, k)
	if err != nil {
		return nil, fmt.Errorf("pwrap: vector: search: %w", err)
	}
	defer rows.Close()

	var out []Match
	for rows.Next() {
		var m Match
		var meta []byte
		if err := rows.Scan(&m.DocID, &meta, &m.Distance); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(meta, &m.Metadata); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// formatVector renders a []float32 as a pgvector literal: "[1,2,3]".
// pgx has no native mapping for `vector`, so we cast `$n::vector` on the SQL side.
func formatVector(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 8)
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
