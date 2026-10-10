-- nexqia-api/migrations/000068_billing_core.down.sql
-- Reverse of 000068: restore 000065 merge function bodies, remove the
-- billing.account.reopen permission, then drop the billing tables.

CREATE OR REPLACE FUNCTION core.person_merge_counts(p_merged uuid) RETURNS jsonb
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

CREATE OR REPLACE FUNCTION core.person_merge_move(p_survivor uuid, p_merged uuid) RETURNS jsonb
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

CREATE OR REPLACE FUNCTION core.person_unmerge_move(p_log_id uuid) RETURNS jsonb
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

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
DELETE FROM core.template_role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000036';
DELETE FROM core.role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000036';
DELETE FROM core.permission WHERE id = '00000000-0000-0000-0004-000000000036';

DROP TABLE operations.billing_consent;
DROP TABLE operations.charge_split;
DROP TABLE operations.charge;
DROP TABLE operations.billing_account;
DROP TABLE operations.guarantee_letter;
ALTER TABLE operations.admission_guarantor DROP COLUMN plan_id, DROP COLUMN coverage_id;
DROP TABLE core.patient_coverage, core.payer_item_coverage, core.payer_plan_limit, core.payer_plan_rule, core.payer_plan;
DROP INDEX core.uq_payer_bpjs_per_merchant;
ALTER TABLE core.payer DROP COLUMN price_list_id;
ALTER TABLE core.merchant DROP CONSTRAINT chk_merchant_bpjs_network, DROP COLUMN default_price_list_id, DROP COLUMN bpjs_network_parent, DROP COLUMN bpjs_cooperation, DROP COLUMN facility_type;
