# pwrap postgres image: pgvector + pg_graphql + PostGIS on top of Postgres 16.
# pgvector comes with the base image. pg_graphql is from the supabase release .debs.
# PostGIS comes from pgdg apt repo, which is already enabled in the base image.
FROM pgvector/pgvector:pg16

ARG PG_GRAPHQL_VERSION=1.5.12

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        curl ca-certificates \
        postgresql-16-postgis-3 \
        postgresql-16-postgis-3-scripts; \
    arch="$(dpkg --print-architecture)"; \
    # Asset naming: pg_graphql-v<VER>-pg16-<arch>-linux-gnu.deb — note the `v` prefix.
    curl -fsSL -o /tmp/pg_graphql.deb \
        "https://github.com/supabase/pg_graphql/releases/download/v${PG_GRAPHQL_VERSION}/pg_graphql-v${PG_GRAPHQL_VERSION}-pg16-${arch}-linux-gnu.deb"; \
    dpkg -i /tmp/pg_graphql.deb; \
    rm /tmp/pg_graphql.deb; \
    apt-get purge -y --auto-remove curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*
