BEGIN;

-- Connect pillar (docs/09-connect-ddl.md §1+§3; spec payung 6/2026 §4 Fase C1).
-- Hanya tabel yang dipakai C1-C3; bpjs_sep/bpjs_claim/referral/guarantee_letter
-- menyusul plan BPJS masing-masing.

CREATE SCHEMA IF NOT EXISTS connect;

-- Hak app_runtime harus dipasang SEBELUM partman membuat partisi awal,
-- agar partisi turut mendapat DEFAULT PRIVILEGES (pola 000002/000026).
GRANT USAGE ON SCHEMA connect TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA connect GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;

CREATE TABLE connect.integration_provider (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code         text NOT NULL UNIQUE,
    name         text NOT NULL,
    protocol     text NOT NULL CHECK (protocol IN ('rest','soap','fhir')),
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE connect.integration_config (
    id                   uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id           uuid NOT NULL REFERENCES core.company(id),
    merchant_id          uuid NOT NULL REFERENCES core.merchant(id),
    provider_id          uuid NOT NULL REFERENCES connect.integration_provider(id),
    base_url             text,
    config               jsonb,
    credential_encrypted bytea,
    is_active            boolean NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    created_by           uuid,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    updated_by           uuid,
    row_version          integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, provider_id)
);
CREATE TRIGGER trg_integration_config_touch BEFORE UPDATE ON connect.integration_config
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE connect.integration_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY integration_config_isolation ON connect.integration_config
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_integration_config_tenant_check BEFORE INSERT OR UPDATE ON connect.integration_config
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE connect.integration_request_log (
    id               uuid NOT NULL DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL,
    merchant_id      uuid NOT NULL,
    provider_id      uuid NOT NULL REFERENCES connect.integration_provider(id),
    endpoint         text NOT NULL,
    request_payload  jsonb,
    response_payload jsonb,
    http_status      integer,
    status           text NOT NULL CHECK (status IN ('success','failed','timeout')),
    duration_ms      integer,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX idx_integration_log_provider ON connect.integration_request_log (provider_id, created_at);
ALTER TABLE connect.integration_request_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY integration_request_log_isolation ON connect.integration_request_log
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_integration_request_log_tenant_check BEFORE INSERT OR UPDATE ON connect.integration_request_log
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
SELECT partman.create_parent(
    p_parent_table => 'connect.integration_request_log',
    p_control      => 'created_at',
    p_interval     => '1 month'
);

-- Penyimpangan disengaja dari docs/09 §3: (1) kolom row_version WAJIB karena
-- trg_touch_row() menaikkannya (000001_shared_prelude.up.sql) dan spec payung
-- §6 mewajibkan pola trg_touch_row -> row_version; (2) status 'sending' +
-- kolom locked_until untuk klaim aman multi-replika (pola email outbox
-- 000052/000061) — worker meng-klaim baris HANYA lewat fungsi SECURITY
-- DEFINER connect.claim_satusehat_maps, tidak pernah query tabel ini
-- langsung dari pool (RLS tenant GUC tidak menempel di koneksi pool).
CREATE TABLE connect.satusehat_resource_map (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    local_table       text NOT NULL,
    local_id          uuid NOT NULL,
    fhir_resource_type text NOT NULL,
    fhir_resource_id   text,
    sync_status        text NOT NULL DEFAULT 'pending'
                       CHECK (sync_status IN ('pending','sending','synced','failed')),
    last_synced_at      timestamptz,
    last_payload        jsonb,
    last_error           text,
    locked_until         timestamptz,
    row_version          integer NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (local_table, local_id, fhir_resource_type)
);
CREATE INDEX idx_satusehat_map_status ON connect.satusehat_resource_map (merchant_id, sync_status);
CREATE TRIGGER trg_satusehat_map_touch BEFORE UPDATE ON connect.satusehat_resource_map
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE connect.satusehat_resource_map ENABLE ROW LEVEL SECURITY;
CREATE POLICY satusehat_map_isolation ON connect.satusehat_resource_map
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_satusehat_resource_map_tenant_check BEFORE INSERT OR UPDATE ON connect.satusehat_resource_map
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

INSERT INTO connect.integration_provider (id, code, name, protocol) VALUES
    ('00000000-0000-0000-0007-000000000001', 'satusehat', 'SATUSEHAT (Kemenkes)', 'fhir')
ON CONFLICT (code) DO NOTHING;

-- Grant blanket DULU (tabel + partisi buatan partman) ...
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA connect TO app_runtime;

-- ... REVOKE penyempitan TERAKHIR agar tidak tertimpa grant blanket di atas
-- (pola urutan 000074:37): katalog global hanya boleh diubah migrasi.
REVOKE INSERT, UPDATE, DELETE ON connect.integration_provider FROM app_runtime;

-- Klaim worker lintas tenant: RLS app.current_merchant_id tidak pernah
-- menempel di koneksi pool (GUC set_config per-tx saja), maka baris hanya
-- terjangkau worker lewat fungsi ini — pola 000052_email_outbox
-- ("the worker serves all companies") + stale-reclaim 000061. Tanpa kolom
-- attempts: baris 'sending' basi di-claim ulang tanpa batas (cap pola 000061
-- menyusul bila pengiriman nyata membutuhkannya).
CREATE FUNCTION connect.claim_satusehat_maps(p_limit integer, p_key text, p_lock_seconds integer)
RETURNS TABLE (id uuid, company_id uuid, merchant_id uuid, local_table text, local_id uuid,
               fhir_resource_type text, fhir_resource_id text, last_payload jsonb,
               base_url text, config jsonb, credential_json text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = connect, pg_temp
AS $$
    WITH picked AS (
        SELECT m.id
          FROM connect.satusehat_resource_map m
          JOIN connect.integration_config c
            ON c.merchant_id = m.merchant_id AND c.is_active
          JOIN connect.integration_provider pr
            ON pr.id = c.provider_id AND pr.code = 'satusehat' AND pr.is_active
         WHERE m.sync_status = 'pending'
            OR (m.sync_status = 'sending' AND m.locked_until < now())
         ORDER BY m.updated_at
         LIMIT p_limit
           FOR UPDATE SKIP LOCKED
    ), claimed AS (
        UPDATE connect.satusehat_resource_map m
           SET sync_status = 'sending',
               locked_until = now() + make_interval(secs => p_lock_seconds)
          FROM picked k
         WHERE m.id = k.id
      RETURNING m.id, m.company_id, m.merchant_id, m.local_table, m.local_id,
                m.fhir_resource_type, m.fhir_resource_id, m.last_payload
    )
    SELECT cl.id, cl.company_id, cl.merchant_id, cl.local_table, cl.local_id,
           cl.fhir_resource_type, cl.fhir_resource_id, cl.last_payload,
           c.base_url, c.config,
           -- pgcrypto terpasang di schema public (000001 tanpa klausa SCHEMA):
           -- kualifikasi eksplisit karena search_path fungsi ini minimum.
           CASE WHEN c.credential_encrypted IS NULL OR p_key = ''
                THEN NULL
                ELSE public.pgp_sym_decrypt(c.credential_encrypted, p_key) END
      FROM claimed cl
      JOIN connect.integration_config c ON c.merchant_id = cl.merchant_id AND c.is_active
      JOIN connect.integration_provider pr ON pr.id = c.provider_id AND pr.code = 'satusehat' AND pr.is_active;
$$;

REVOKE ALL ON FUNCTION connect.claim_satusehat_maps(integer, text, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION connect.claim_satusehat_maps(integer, text, integer) TO app_runtime;

COMMIT;
