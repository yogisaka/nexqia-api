-- nexqia-api/migrations/000065_person_merge.up.sql
-- Spec 2026-10-07-merge-medical-records §4, §9.

-- 4.1 person
ALTER TABLE core.person ADD COLUMN merged_into_person_id uuid REFERENCES core.person(id);
ALTER TABLE core.person ADD CONSTRAINT chk_person_merged_deleted
    CHECK (merged_into_person_id IS NULL OR deleted_at IS NOT NULL);
ALTER TABLE core.person ADD CONSTRAINT chk_person_merged_not_self
    CHECK (merged_into_person_id IS NULL OR merged_into_person_id <> id);
CREATE INDEX idx_person_merged_mrn ON core.person (company_id, medical_record_no)
    WHERE merged_into_person_id IS NOT NULL;

-- 4.2 merge log
ALTER TABLE core.person_merge_log
    ADD COLUMN action text NOT NULL DEFAULT 'merge' CHECK (action IN ('merge', 'unmerge')),
    ADD COLUMN merge_log_id uuid REFERENCES core.person_merge_log(id),
    ADD COLUMN identity_check text CHECK (identity_check IN ('ok', 'same_identity', 'needs_documents')),
    ADD COLUMN verified_documents text[] NOT NULL DEFAULT '{}'
        CHECK (verified_documents <@ ARRAY['ktp', 'kk', 'akta_lahir', 'kartu_bpjs', 'paspor', 'kitas']::text[]),
    ADD COLUMN field_choices jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN snapshot jsonb,
    ADD COLUMN moved jsonb,
    ADD COLUMN reverted_at timestamptz,
    ADD COLUMN reverted_by uuid,
    ADD CONSTRAINT chk_person_merge_log_unmerge_ref CHECK ((action = 'unmerge') = (merge_log_id IS NOT NULL));
CREATE INDEX idx_person_merge_log_company ON core.person_merge_log (company_id, merged_at DESC);

-- 4.3 allergy review flag
ALTER TABLE core.patient_allergy ADD COLUMN needs_review boolean NOT NULL DEFAULT false;

-- 4.4 duplicate pairs
CREATE TABLE core.person_duplicate_pair (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    person_a_id uuid NOT NULL REFERENCES core.person(id),
    person_b_id uuid NOT NULL REFERENCES core.person(id),
    status      text NOT NULL CHECK (status IN ('requested', 'dismissed', 'merged')),
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid,
    row_version integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_person_duplicate_pair_order CHECK (person_a_id < person_b_id),
    UNIQUE (person_a_id, person_b_id)
);
CREATE TRIGGER trg_person_duplicate_pair_touch BEFORE UPDATE ON core.person_duplicate_pair
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE FUNCTION core.trg_fill_tenant_person_duplicate_pair() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id := (SELECT p.company_id FROM core.person p WHERE p.id = NEW.person_a_id);
    IF NOT EXISTS (SELECT 1 FROM core.person p WHERE p.id = NEW.person_b_id AND p.company_id = NEW.company_id) THEN
        RAISE EXCEPTION 'duplicate pair across companies is not allowed' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON core.person_duplicate_pair
    FOR EACH ROW EXECUTE FUNCTION core.trg_fill_tenant_person_duplicate_pair();
ALTER TABLE core.person_duplicate_pair ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_duplicate_pair_isolation ON core.person_duplicate_pair
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- 4.5 no new rows for a merged person (closes the read-then-insert race)
CREATE FUNCTION core.trg_person_not_merged() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM core.person p WHERE p.id = NEW.person_id AND p.merged_into_person_id IS NOT NULL) THEN
        RAISE EXCEPTION 'person has been merged' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON operations.admission
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON operations.queue
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON operations.queue_journey
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON core.patient_allergy
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON core.person_family
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();

-- 4.6 cross-merchant helpers (RLS on admission/queue is per merchant; a
-- person is company-wide). Every function is scoped to app.current_company_id.
CREATE FUNCTION core.person_merge_assert(p_ids uuid[]) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
DECLARE
    v_company uuid := NULLIF(current_setting('app.current_company_id', true), '')::uuid;
BEGIN
    IF v_company IS NULL OR (SELECT count(DISTINCT p.id) FROM core.person p
                              WHERE p.id = ANY(p_ids) AND p.company_id = v_company) <> cardinality(p_ids) THEN
        RAISE EXCEPTION 'person not found in the current company' USING ERRCODE = 'no_data_found';
    END IF;
    RETURN v_company;
END;
$$;

CREATE FUNCTION core.person_merge_blockers(p_merged uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, operations, pg_temp STABLE AS $$
BEGIN
    PERFORM core.person_merge_assert(ARRAY[p_merged]);
    RETURN jsonb_build_object(
        'active_visits',
            (SELECT count(*) FROM operations.admission a
              WHERE a.person_id = p_merged AND a.deleted_at IS NULL
                AND a.status IN ('registered', 'admitted', 'in_treatment'))
          + (SELECT count(*) FROM operations.queue q
              WHERE q.person_id = p_merged AND q.status IN ('waiting', 'called', 'in_progress'))
          + (SELECT count(*) FROM operations.queue_journey j
              WHERE j.person_id = p_merged AND j.status IN ('in_progress', 'awaiting_checkin')),
        'linked_account', EXISTS (SELECT 1 FROM core.app_user u WHERE u.person_id = p_merged AND u.deleted_at IS NULL),
        'linked_physician', EXISTS (SELECT 1 FROM core.physician ph WHERE ph.person_id = p_merged AND ph.deleted_at IS NULL));
END;
$$;

CREATE FUNCTION core.person_merge_counts(p_merged uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, operations, pg_temp STABLE AS $$
DECLARE
    v_company uuid := core.person_merge_assert(ARRAY[p_merged]);
BEGIN
    RETURN jsonb_build_object(
        'operations.admission', (SELECT count(*) FROM operations.admission WHERE person_id = p_merged),
        'operations.queue', (SELECT count(*) FROM operations.queue WHERE person_id = p_merged),
        'operations.queue_journey', (SELECT count(*) FROM operations.queue_journey WHERE person_id = p_merged),
        'core.patient_allergy', (SELECT count(*) FROM core.patient_allergy WHERE person_id = p_merged),
        'core.person_family', (SELECT count(*) FROM core.person_family WHERE person_id = p_merged),
        'core.media_link', (SELECT count(*) FROM core.media_link WHERE owner_table = 'person' AND owner_id = p_merged),
        'core.person.family_id', (SELECT count(*) FROM core.person WHERE family_id = p_merged AND company_id = v_company));
END;
$$;

CREATE FUNCTION core.person_merge_move(p_survivor uuid, p_merged uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, operations, pg_temp AS $$
DECLARE
    v_company uuid := core.person_merge_assert(ARRAY[p_survivor, p_merged]);
    v_blockers jsonb;
    v_moved jsonb := '{}';
    v_ids uuid[];
BEGIN
    IF p_survivor = p_merged OR (SELECT count(*) FROM core.person
                                  WHERE id IN (p_survivor, p_merged) AND deleted_at IS NULL) <> 2 THEN
        RAISE EXCEPTION 'merge needs two different live records' USING ERRCODE = 'check_violation';
    END IF;
    v_blockers := core.person_merge_blockers(p_merged);
    IF (v_blockers->>'active_visits')::int > 0 OR (v_blockers->>'linked_account')::boolean
       OR (v_blockers->>'linked_physician')::boolean THEN
        RAISE EXCEPTION 'merged record is blocked: %', v_blockers USING ERRCODE = 'check_violation';
    END IF;

    WITH u AS (UPDATE operations.admission SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('operations.admission', to_jsonb(v_ids));
    WITH u AS (UPDATE operations.queue SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('operations.queue', to_jsonb(v_ids));
    WITH u AS (UPDATE operations.queue_journey SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('operations.queue_journey', to_jsonb(v_ids));
    WITH u AS (UPDATE core.patient_allergy SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('core.patient_allergy', to_jsonb(v_ids));
    WITH u AS (UPDATE core.person_family SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('core.person_family', to_jsonb(v_ids));
    WITH u AS (UPDATE core.media_link SET owner_id = p_survivor
                WHERE owner_table = 'person' AND owner_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('core.media_link', to_jsonb(v_ids));
    WITH u AS (UPDATE core.person SET family_id = CASE WHEN id = p_survivor THEN NULL ELSE p_survivor END
                WHERE family_id = p_merged AND company_id = v_company RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('core.person.family_id', to_jsonb(v_ids));

    -- same substance recorded on both records → flag for review, keep both (M4)
    UPDATE core.patient_allergy a SET needs_review = true
    WHERE a.person_id = p_survivor AND a.is_active AND EXISTS (
        SELECT 1 FROM core.patient_allergy b
        WHERE b.person_id = p_survivor AND b.is_active AND b.id <> a.id
          AND b.allergy_type = a.allergy_type
          AND lower(btrim(b.substance_name)) = lower(btrim(a.substance_name)));
    RETURN v_moved;
END;
$$;

CREATE FUNCTION core.person_unmerge_move(p_log_id uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, operations, pg_temp AS $$
DECLARE
    v_company uuid := NULLIF(current_setting('app.current_company_id', true), '')::uuid;
    v_log core.person_merge_log%ROWTYPE;
    v_s uuid;
    v_m uuid;
    v_ids uuid[];
    v_n int;
    v_out jsonb := '{}';
BEGIN
    SELECT * INTO v_log FROM core.person_merge_log
     WHERE id = p_log_id AND company_id = v_company AND action = 'merge' AND reverted_at IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'merge log not found or already reverted' USING ERRCODE = 'no_data_found';
    END IF;
    v_s := v_log.surviving_person_id;
    v_m := v_log.merged_person_id;
    IF EXISTS (SELECT 1 FROM core.person WHERE id = v_m AND (deleted_at IS NOT NULL OR merged_into_person_id IS NOT NULL)) THEN
        RAISE EXCEPTION 'restore the merged record before moving rows back' USING ERRCODE = 'check_violation';
    END IF;

    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'operations.admission', '[]'))::uuid);
    UPDATE operations.admission SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('operations.admission', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'operations.queue', '[]'))::uuid);
    UPDATE operations.queue SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('operations.queue', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'operations.queue_journey', '[]'))::uuid);
    UPDATE operations.queue_journey SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('operations.queue_journey', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'core.patient_allergy', '[]'))::uuid);
    UPDATE core.patient_allergy SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('core.patient_allergy', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'core.person_family', '[]'))::uuid);
    UPDATE core.person_family SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('core.person_family', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'core.media_link', '[]'))::uuid);
    UPDATE core.media_link SET owner_id = v_m WHERE id = ANY(v_ids) AND owner_table = 'person' AND owner_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('core.media_link', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'core.person.family_id', '[]'))::uuid);
    UPDATE core.person SET family_id = v_m
     WHERE id = ANY(v_ids) AND company_id = v_company
       AND ((id = v_s AND family_id IS NULL) OR (id <> v_s AND family_id = v_s));
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('core.person.family_id', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
    RETURN v_out;
END;
$$;

GRANT EXECUTE ON FUNCTION core.person_merge_assert(uuid[]) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.person_merge_blockers(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.person_merge_counts(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.person_merge_move(uuid, uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.person_unmerge_move(uuid) TO app_runtime;

-- §9 permissions (id pattern of 000043; 030/031 taken by 000055/000057)
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000032', 'core.person.merge_request', 'Ajukan gabung RM duplikat', 'core'),
    ('00000000-0000-0000-0004-000000000033', 'core.person.merge', 'Gabungkan RM duplikat', 'core'),
    ('00000000-0000-0000-0004-000000000034', 'core.person.unmerge', 'Batalkan gabung RM', 'core')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r JOIN core.permission p
  ON p.code IN ('core.person.merge_request', 'core.person.merge', 'core.person.unmerge')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- platform role templates (version 3 → 4)
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('core.person.merge_request', 'core.person.merge', 'core.person.unmerge')
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'core.person.merge_request'
WHERE tr.name = 'Pendaftaran'
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role (template_id, name, description, requires_physician_data, sort_order)
SELECT t.id, 'Rekam Medis', 'Petugas rekam medis (PMIK): kelola data pasien dan gabung RM duplikat', false,
       (SELECT max(tr.sort_order) + 1 FROM core.template_role tr WHERE tr.template_id = t.id)
FROM core.template t
WHERE t.scope = 'platform' AND t.kind = 'role' AND t.code IN ('klinik_utama', 'rumah_sakit')
ON CONFLICT (template_id, name) DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('core.person.manage', 'core.person.merge_request', 'core.person.merge', 'clinical.record.view')
WHERE tr.name = 'Rekam Medis'
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
