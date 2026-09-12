FROM postgres:18

# pg_partman: required by migrations/000001_shared_prelude.up.sql (CREATE EXTENSION pg_partman).
# Kept in sync with nexqia/infra/postgres/Dockerfile — same Postgres major version, same package.
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-18-partman \
    && rm -rf /var/lib/apt/lists/*
