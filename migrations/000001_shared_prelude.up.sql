-- migrations/000001_shared_prelude.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §0 "Shared prelude" — runs once, used by every pillar.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS pg_partman;

CREATE OR REPLACE FUNCTION uuid_generate_v7()
RETURNS uuid AS $$
    SELECT encode(
        set_bit(
            set_bit(
                overlay(uuid_send(gen_random_uuid()) placing
                    substring(int8send(floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint) from 3)
                    from 1 for 6),
                52, 1),
            53, 1),
        'hex')::uuid;
$$ LANGUAGE sql VOLATILE;

CREATE OR REPLACE FUNCTION trg_touch_row()
RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION trg_check_tenant_consistency()
RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM core.merchant m
        WHERE m.id = NEW.merchant_id AND m.company_id = NEW.company_id
    ) THEN
        RAISE EXCEPTION 'tenant mismatch: merchant_id % bukan milik company_id %', NEW.merchant_id, NEW.company_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS terminology;
