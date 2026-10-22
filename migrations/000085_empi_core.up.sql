BEGIN;

-- EMPI: identitas pasien lintas company (spec 2026-10-20-empi-cross-company §5).
-- Tabel enterprise scope: TANPA RLS dan TANPA trg_check_tenant_consistency
-- (pengecualian aturan tabel-tenant; preseden katalog global 000077:112-117
-- dan schema platform 000039:2-7). Akses dijaga permission empi.* (G2) dan
-- hanya fungsi ingest internal yang menulis di G1. Golden = first-non-empty
-- dari link aktif menurut urutan person_link.id (uuidv7); manual_fields menang.

CREATE SCHEMA empi;

GRANT USAGE ON SCHEMA empi TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA empi GRANT SELECT, INSERT, UPDATE ON TABLES TO app_runtime;

CREATE TABLE empi.master_person (
    id                             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    full_name                      text NOT NULL DEFAULT '',
    birth_date                     date,
    gender                         text,
    full_name_source_person_id     uuid,
    birth_date_source_person_id    uuid,
    gender_source_person_id        uuid,
    manual_fields                  text[] NOT NULL DEFAULT '{}'
                                   CHECK (manual_fields <@ ARRAY['full_name','birth_date','gender']::text[]),
    merged_into_master_person_id   uuid REFERENCES empi.master_person(id),
    row_version                    integer NOT NULL DEFAULT 1,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    updated_at                     timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_master_person_touch BEFORE UPDATE ON empi.master_person
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

CREATE TABLE empi.person_link (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    master_person_id   uuid NOT NULL REFERENCES empi.master_person(id),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    person_id          uuid NOT NULL REFERENCES core.person(id),
    nik_search_hash    text,
    full_name          text NOT NULL DEFAULT '',
    birth_date         date,
    gender             text,
    phone              text,
    status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active','detached')),
    source_updated_at  timestamptz NOT NULL DEFAULT now(),
    row_version        integer NOT NULL DEFAULT 1,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, person_id)
);
CREATE TRIGGER trg_person_link_touch BEFORE UPDATE ON empi.person_link
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE INDEX idx_person_link_nik ON empi.person_link (nik_search_hash)
    WHERE nik_search_hash IS NOT NULL;
CREATE INDEX idx_person_link_birth ON empi.person_link (birth_date);
CREATE INDEX idx_person_link_phone ON empi.person_link (phone)
    WHERE phone IS NOT NULL AND phone <> '';

CREATE TABLE empi.candidate_pair (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    person_a_id         uuid NOT NULL REFERENCES core.person(id),
    person_b_id         uuid NOT NULL REFERENCES core.person(id),
    person_a_company_id uuid NOT NULL REFERENCES core.company(id),
    person_b_company_id uuid NOT NULL REFERENCES core.company(id),
    score               integer NOT NULL,
    reason              text NOT NULL CHECK (reason IN ('same_nik','birth_gender_name','phone_name')),
    status              text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','dismissed','linked')),
    note                text,
    created_by          uuid,
    decided_by          uuid,
    decided_at          timestamptz,
    row_version         integer NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (person_a_id, person_b_id),
    CHECK (person_a_company_id <> person_b_company_id)
);
CREATE TRIGGER trg_candidate_pair_touch BEFORE UPDATE ON empi.candidate_pair
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

CREATE TABLE empi.merge_log (
    id                        uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    winner_master_person_id   uuid NOT NULL REFERENCES empi.master_person(id),
    loser_master_person_id    uuid NOT NULL REFERENCES empi.master_person(id),
    snapshot                  jsonb NOT NULL,
    reason                    text,
    merged_by                 uuid NOT NULL,
    merged_at                 timestamptz NOT NULL DEFAULT now(),
    unmerged_by               uuid,
    unmerged_at               timestamptz,
    row_version               integer NOT NULL DEFAULT 1,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (winner_master_person_id <> loser_master_person_id)
);
CREATE TRIGGER trg_merge_log_touch BEFORE UPDATE ON empi.merge_log
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA empi TO app_runtime;

-- Izin EMPI (amandemen spec §5.5 2026-10-20, Opsi 1): katalog SAJA — TANPA
-- role/template penugasan, TANPA bump versi (operator memberi manual via
-- Role Matrix saat G2; keputusan user #6: tanpa akses default staf company).
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000049', 'empi.master.view',
     'Melihat identitas EMPI lintas company (resolve)', 'empi'),
    ('00000000-0000-0000-0004-00000000004a', 'empi.master.manage',
     'Review kandidat, merge/un-merge master EMPI', 'empi')
ON CONFLICT (id) DO NOTHING;

COMMIT;
