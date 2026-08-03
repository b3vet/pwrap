import type { Sql } from "./client.js";

export interface Feature {
  id: string;
  geometry: Record<string, unknown>;
  metadata: Record<string, unknown>;
  created_at: Date;
  updated_at: Date;
  /** Populated by withinRadius and nearest; zero otherwise. */
  distance_meters?: number;
}

/** PostGIS-backed feature collection. Geometries are stored in EPSG:4326 (WGS84). */
export class Geo {
  constructor(private sql: Sql, private collection: string) {}

  /**
   * Bulk-insert a batch of (lng, lat) points in a single round-trip.
   * Returns generated ids in input order. Encoded as a single jsonb batch param
   * for postgres.js compatibility.
   */
  async insertPointMany(points: { lng: number; lat: number; metadata?: Record<string, unknown> }[]): Promise<string[]> {
    if (points.length === 0) return [];
    const payload = points.map((p) => ({ lng: p.lng, lat: p.lat, metadata: p.metadata ?? {} }));
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const json = this.sql.json(payload as any);
    const rows = await this.sql<{ id: string }[]>`
      INSERT INTO pwrap_geo (collection, geom, metadata)
      SELECT ${this.collection},
             ST_SetSRID(ST_MakePoint((rec->>'lng')::float8, (rec->>'lat')::float8), 4326),
             rec->'metadata'
        FROM jsonb_array_elements(${json}::jsonb) WITH ORDINALITY AS u(rec, ord)
       ORDER BY ord
      RETURNING id
    `;
    return rows.map((r) => r.id);
  }

  /** Insert a single (lng, lat) point. */
  async insertPoint(lng: number, lat: number, metadata: Record<string, unknown> = {}): Promise<string> {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const meta = this.sql.json(metadata as any);
    const rows = await this.sql<{ id: string }[]>`
      INSERT INTO pwrap_geo (collection, geom, metadata)
      VALUES (${this.collection}, ST_SetSRID(ST_MakePoint(${lng}, ${lat}), 4326), ${meta})
      RETURNING id
    `;
    return rows[0].id;
  }

  /** Insert any GeoJSON geometry (Point, LineString, Polygon, Multi*, GeometryCollection). */
  async insert(geoJSON: Record<string, unknown>, metadata: Record<string, unknown> = {}): Promise<string> {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const meta = this.sql.json(metadata as any);
    const geomText = JSON.stringify(geoJSON);
    const rows = await this.sql<{ id: string }[]>`
      INSERT INTO pwrap_geo (collection, geom, metadata)
      VALUES (${this.collection}, ST_SetSRID(ST_GeomFromGeoJSON(${geomText}), 4326), ${meta})
      RETURNING id
    `;
    return rows[0].id;
  }

  async get(id: string): Promise<Feature | null> {
    const rows = await this.sql<Feature[]>`
      SELECT id,
             ST_AsGeoJSON(geom)::jsonb AS geometry,
             metadata,
             created_at,
             updated_at
      FROM pwrap_geo
      WHERE collection = ${this.collection} AND id = ${id}
    `;
    return rows[0] ?? null;
  }

  async delete(id: string): Promise<boolean> {
    const res = await this.sql`
      DELETE FROM pwrap_geo WHERE collection = ${this.collection} AND id = ${id}
    `;
    return res.count > 0;
  }

  /** Features within `meters` of (lng, lat), sorted by distance. */
  async withinRadius(lng: number, lat: number, meters: number, limit = 100): Promise<Feature[]> {
    const rows = await this.sql<Feature[]>`
      SELECT id,
             ST_AsGeoJSON(geom)::jsonb              AS geometry,
             metadata,
             created_at,
             updated_at,
             ST_Distance(
                 geom::geography,
                 ST_SetSRID(ST_MakePoint(${lng}, ${lat}), 4326)::geography
             )                                       AS distance_meters
      FROM pwrap_geo
      WHERE collection = ${this.collection}
        AND ST_DWithin(
            geom::geography,
            ST_SetSRID(ST_MakePoint(${lng}, ${lat}), 4326)::geography,
            ${meters}
        )
      ORDER BY distance_meters
      LIMIT ${limit}
    `;
    return rows.map((r) => ({ ...r, distance_meters: Number(r.distance_meters) }));
  }

  /** Features whose bounding box intersects the envelope. Fast (GIST), no distance. */
  async withinBBox(minLng: number, minLat: number, maxLng: number, maxLat: number, limit = 100): Promise<Feature[]> {
    return this.sql<Feature[]>`
      SELECT id,
             ST_AsGeoJSON(geom)::jsonb AS geometry,
             metadata,
             created_at,
             updated_at
      FROM pwrap_geo
      WHERE collection = ${this.collection}
        AND geom && ST_MakeEnvelope(${minLng}, ${minLat}, ${maxLng}, ${maxLat}, 4326)
      LIMIT ${limit}
    `;
  }

  /** K nearest features to (lng, lat), ordered by KNN distance, with meter distance attached. */
  async nearest(lng: number, lat: number, k = 10): Promise<Feature[]> {
    const rows = await this.sql<Feature[]>`
      SELECT id,
             ST_AsGeoJSON(geom)::jsonb AS geometry,
             metadata,
             created_at,
             updated_at,
             ST_Distance(
                 geom::geography,
                 ST_SetSRID(ST_MakePoint(${lng}, ${lat}), 4326)::geography
             )                          AS distance_meters
      FROM pwrap_geo
      WHERE collection = ${this.collection}
      ORDER BY geom <-> ST_SetSRID(ST_MakePoint(${lng}, ${lat}), 4326)
      LIMIT ${k}
    `;
    return rows.map((r) => ({ ...r, distance_meters: Number(r.distance_meters) }));
  }

  async count(): Promise<number> {
    const rows = await this.sql<{ count: bigint }[]>`
      SELECT COUNT(*) FROM pwrap_geo WHERE collection = ${this.collection}
    `;
    return Number(rows[0].count);
  }
}
