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

// Feature is one row from a geo collection.
//
// Geometry is the GeoJSON representation of the stored geometry. The SDK never imposes
// a Go-side geometry type — callers can decode it with any GeoJSON library, or just
// inspect the raw map.
type Feature struct {
	ID         uuid.UUID      `json:"id"`
	Geometry   map[string]any `json:"geometry"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	// DistanceMeters is populated by WithinRadius and Nearest. Zero for plain Find/Get/BBox.
	DistanceMeters float64 `json:"distance_meters,omitempty"`
}

// Geo is a handle for a named PostGIS-backed feature collection.
type Geo struct {
	client     *Client
	collection string
}

// Geo returns a handle for the named collection. Geometries are stored in EPSG:4326
// (WGS84). Mix points, lines, polygons in the same collection — the table accepts
// any GEOMETRY subtype.
func (c *Client) Geo(collection string) *Geo {
	return &Geo{client: c, collection: collection}
}

// GeoPoint is one (lng, lat, metadata) tuple for InsertPointMany.
type GeoPoint struct {
	Lng      float64
	Lat      float64
	Metadata map[string]any
}

// InsertPointMany inserts a batch of points in a single round-trip and returns the
// generated ids in the same order. Built on `unnest()` so batch size isn't bounded
// by Postgres's parameter limit.
//
// Empty input is a no-op (returns nil, nil).
func (g *Geo) InsertPointMany(ctx context.Context, points []GeoPoint) ([]uuid.UUID, error) {
	if len(points) == 0 {
		return nil, nil
	}
	lngs := make([]float64, len(points))
	lats := make([]float64, len(points))
	metas := make([]string, len(points))
	for i, p := range points {
		lngs[i] = p.Lng
		lats[i] = p.Lat
		m, err := json.Marshal(orEmptyMap(p.Metadata))
		if err != nil {
			return nil, err
		}
		metas[i] = string(m)
	}
	ids := make([]uuid.UUID, 0, len(points))
	err := g.client.run(ctx, func(ctx context.Context, q querier) error {
		rows, err := q.Query(ctx, `
			INSERT INTO pwrap_geo (collection, geom, metadata)
			SELECT $1, ST_SetSRID(ST_MakePoint(lng, lat), 4326), meta::jsonb
			  FROM unnest($2::float8[], $3::float8[], $4::text[]) WITH ORDINALITY AS u(lng, lat, meta, ord)
			 ORDER BY ord
			RETURNING id
		`, g.collection, lngs, lats, metas)
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

// InsertPoint stores a single (lng, lat) point with optional metadata. Returns the id.
// Convenience wrapper around Insert(GeoJSON); the most common pattern.
func (g *Geo) InsertPoint(ctx context.Context, lng, lat float64, metadata map[string]any) (uuid.UUID, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = g.client.run(ctx, func(ctx context.Context, q querier) error {
		return q.QueryRow(ctx, `
			INSERT INTO pwrap_geo (collection, geom, metadata)
			VALUES ($1, ST_SetSRID(ST_MakePoint($2, $3), 4326), $4::jsonb)
			RETURNING id
		`, g.collection, lng, lat, metaJSON).Scan(&id)
	})
	return id, err
}

// Insert stores any GeoJSON geometry — point, line, polygon, multi-*, geometry collection.
// Pass the geometry as a JSON-encoded GeoJSON object: {"type":"Point","coordinates":[...]}.
func (g *Geo) Insert(ctx context.Context, geoJSON string, metadata map[string]any) (uuid.UUID, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = g.client.run(ctx, func(ctx context.Context, q querier) error {
		return q.QueryRow(ctx, `
			INSERT INTO pwrap_geo (collection, geom, metadata)
			VALUES ($1, ST_SetSRID(ST_GeomFromGeoJSON($2), 4326), $3::jsonb)
			RETURNING id
		`, g.collection, geoJSON, metaJSON).Scan(&id)
	})
	return id, err
}

// Get fetches a single feature by id.
func (g *Geo) Get(ctx context.Context, id uuid.UUID) (Feature, error) {
	var f Feature
	var geomJSON, metaJSON []byte
	err := g.client.run(ctx, func(ctx context.Context, q querier) error {
		err := q.QueryRow(ctx, `
			SELECT id, ST_AsGeoJSON(geom)::text, metadata::text, created_at, updated_at
			FROM pwrap_geo
			WHERE collection = $1 AND id = $2
		`, g.collection, id).Scan(&f.ID, &geomJSON, &metaJSON, &f.CreatedAt, &f.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return Feature{}, err
	}
	if err := json.Unmarshal(geomJSON, &f.Geometry); err != nil {
		return Feature{}, fmt.Errorf("pwrap: geo: decode geometry: %w", err)
	}
	if err := json.Unmarshal(metaJSON, &f.Metadata); err != nil {
		return Feature{}, fmt.Errorf("pwrap: geo: decode metadata: %w", err)
	}
	return f, nil
}

// Delete removes a feature by id.
func (g *Geo) Delete(ctx context.Context, id uuid.UUID) error {
	var affected int64
	err := g.client.run(ctx, func(ctx context.Context, q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM pwrap_geo WHERE collection = $1 AND id = $2`, g.collection, id)
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

// WithinRadius returns features whose geometry is within `meters` of the given point.
// Distance is computed on the spheroid (geography cast). Results are ordered by distance.
// If limit <= 0, defaults to 100.
func (g *Geo) WithinRadius(ctx context.Context, lng, lat, meters float64, limit int) ([]Feature, error) {
	if limit <= 0 {
		limit = 100
	}
	return g.scanFeatures(ctx, `
		SELECT id,
		       ST_AsGeoJSON(geom)::text,
		       metadata::text,
		       created_at,
		       updated_at,
		       ST_Distance(
		           geom::geography,
		           ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
		       ) AS distance_meters
		FROM pwrap_geo
		WHERE collection = $3
		  AND ST_DWithin(
		      geom::geography,
		      ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography,
		      $4
		  )
		ORDER BY distance_meters
		LIMIT $5
	`, true, lng, lat, g.collection, meters, limit)
}

// WithinBBox returns features whose bounding box intersects the given lng/lat envelope.
// Uses the GIST index for fast rectangular filtering. No ordering / no distance.
// If limit <= 0, defaults to 100.
func (g *Geo) WithinBBox(ctx context.Context, minLng, minLat, maxLng, maxLat float64, limit int) ([]Feature, error) {
	if limit <= 0 {
		limit = 100
	}
	return g.scanFeatures(ctx, `
		SELECT id, ST_AsGeoJSON(geom)::text, metadata::text, created_at, updated_at, 0::double precision
		FROM pwrap_geo
		WHERE collection = $1
		  AND geom && ST_MakeEnvelope($2, $3, $4, $5, 4326)
		LIMIT $6
	`, false, g.collection, minLng, minLat, maxLng, maxLat, limit)
}

// Nearest returns the k features nearest to (lng, lat) using PostGIS KNN (`<->`),
// then re-computes meter-accurate distance for each result.
// If k <= 0, defaults to 10.
func (g *Geo) Nearest(ctx context.Context, lng, lat float64, k int) ([]Feature, error) {
	if k <= 0 {
		k = 10
	}
	return g.scanFeatures(ctx, `
		SELECT id,
		       ST_AsGeoJSON(geom)::text,
		       metadata::text,
		       created_at,
		       updated_at,
		       ST_Distance(
		           geom::geography,
		           ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
		       ) AS distance_meters
		FROM pwrap_geo
		WHERE collection = $3
		ORDER BY geom <-> ST_SetSRID(ST_MakePoint($1, $2), 4326)
		LIMIT $4
	`, true, lng, lat, g.collection, k)
}

// Count returns how many features the collection holds.
func (g *Geo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := g.client.run(ctx, func(ctx context.Context, q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM pwrap_geo WHERE collection = $1`, g.collection).Scan(&n)
	})
	return n, err
}

// --- internal ---------------------------------------------------------------

func (g *Geo) scanFeatures(ctx context.Context, sql string, withDistance bool, args ...any) ([]Feature, error) {
	var out []Feature
	err := g.client.run(ctx, func(ctx context.Context, q querier) error {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Feature
			var geomStr, metaStr string
			var dist float64
			if err := rows.Scan(&f.ID, &geomStr, &metaStr, &f.CreatedAt, &f.UpdatedAt, &dist); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(geomStr), &f.Geometry); err != nil {
				return fmt.Errorf("pwrap: geo: decode geometry: %w", err)
			}
			if err := json.Unmarshal([]byte(metaStr), &f.Metadata); err != nil {
				return fmt.Errorf("pwrap: geo: decode metadata: %w", err)
			}
			if withDistance {
				f.DistanceMeters = dist
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}
