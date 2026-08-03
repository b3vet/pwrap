# pwrap postgres image: pgvector + pg_graphql + PostGIS.
# pgvector comes with the base image. pg_graphql is from the supabase release .debs.
# PostGIS comes from the pgdg apt repo, already enabled in the base image.
#
# PG_VERSION selects the major version end to end — base image, PostGIS packages
# and the pg_graphql asset all follow it, so the nightly matrix can build the
# same image against every Postgres pwrap claims to support.
ARG PG_VERSION=16
FROM pgvector/pgvector:pg${PG_VERSION}

# ARGs declared before FROM are only visible to FROM; re-declare to use it below.
ARG PG_VERSION
ARG PG_GRAPHQL_VERSION=1.5.12

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        curl ca-certificates \
        postgresql-${PG_VERSION}-postgis-3 \
        postgresql-${PG_VERSION}-postgis-3-scripts; \
    arch="$(dpkg --print-architecture)"; \
    # Asset naming: pg_graphql-v<VER>-pg<MAJOR>-<arch>-linux-gnu.deb — note the `v` prefix.
    curl -fsSL -o /tmp/pg_graphql.deb \
        "https://github.com/supabase/pg_graphql/releases/download/v${PG_GRAPHQL_VERSION}/pg_graphql-v${PG_GRAPHQL_VERSION}-pg${PG_VERSION}-${arch}-linux-gnu.deb"; \
    dpkg -i /tmp/pg_graphql.deb; \
    rm /tmp/pg_graphql.deb; \
    apt-get purge -y --auto-remove curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*
