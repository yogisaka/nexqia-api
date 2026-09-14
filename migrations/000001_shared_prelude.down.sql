-- migrations/000001_shared_prelude.down.sql
DROP SCHEMA IF EXISTS terminology CASCADE;
DROP SCHEMA IF EXISTS core CASCADE;
DROP FUNCTION IF EXISTS trg_check_tenant_consistency();
DROP FUNCTION IF EXISTS trg_touch_row();
DROP FUNCTION IF EXISTS uuid_generate_v7();
DROP EXTENSION IF EXISTS pg_partman;
DROP SCHEMA IF EXISTS partman;
DROP EXTENSION IF EXISTS pg_stat_statements;
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS pgcrypto;
