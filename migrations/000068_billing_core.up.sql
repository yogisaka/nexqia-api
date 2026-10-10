-- nexqia-api/migrations/000068_billing_core.up.sql
-- Spec 2026-10-07-tariff-b1-billing-core §3: facility BPJS profile, payer plans,
-- BPJS item coverage, patient coverage, guarantee letters, billing account,
-- charges, charge splits and out-of-benefit consent.

ALTER TABLE core.merchant
    ADD COLUMN facility_type text CHECK (facility_type IN ('klinik_pratama','klinik_utama','praktik_dokter','praktik_dokter_gigi','praktik_bidan','puskesmas','rumah_sakit')),
    ADD COLUMN bpjs_cooperation text NOT NULL DEFAULT 'none' CHECK (bpjs_cooperation IN ('none','fktp_capitation','fktp_network','fkrtl')),
    ADD COLUMN bpjs_network_parent text,
    ADD COLUMN default_price_list_id uuid REFERENCES core.price_list(id),
    ADD CONSTRAINT chk_merchant_bpjs_network CHECK (bpjs_cooperation <> 'fktp_network' OR btrim(coalesce(bpjs_network_parent, '')) <> '');

ALTER TABLE core.payer ADD COLUMN price_list_id uuid REFERENCES core.price_list(id);
CREATE UNIQUE INDEX uq_payer_bpjs_per_merchant ON core.payer (merchant_id) WHERE payer_type = 'bpjs' AND deleted_at IS NULL;

CREATE TABLE core.payer_plan (
    id                    uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id            uuid NOT NULL REFERENCES core.company(id),
    merchant_id           uuid NOT NULL REFERENCES core.merchant(id),
    payer_id              uuid NOT NULL REFERENCES core.payer(id),
    code                  text NOT NULL CHECK (btrim(code) <> ''),
    name                  text NOT NULL CHECK (btrim(name) <> ''),
    requires_gl           boolean NOT NULL DEFAULT false,
    annual_limit          numeric(15,2) CHECK (annual_limit > 0),
    plan_year_start_month smallint NOT NULL DEFAULT 1 CHECK (plan_year_start_month BETWEEN 1 AND 12),
    is_active             boolean NOT NULL DEFAULT true,
    created_at            timestamptz NOT NULL DEFAULT now(),
    created_by            uuid,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    updated_by            uuid,
    deleted_at            timestamptz,
    deleted_by            uuid,
    row_version           integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_payer_plan_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_payer_plan_code ON core.payer_plan (payer_id, code) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_payer_plan_touch BEFORE UPDATE ON core.payer_plan FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.payer_plan ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_plan_isolation ON core.payer_plan USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_plan_tenant_check BEFORE INSERT OR UPDATE ON core.payer_plan FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.payer_plan_rule (
    id                   uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id           uuid NOT NULL REFERENCES core.company(id),
    merchant_id          uuid NOT NULL REFERENCES core.merchant(id),
    plan_id              uuid NOT NULL REFERENCES core.payer_plan(id) ON DELETE CASCADE,
    care_type            text NOT NULL CHECK (care_type IN ('outpatient','inpatient','emergency')),
    copay_percent        numeric(5,2) NOT NULL DEFAULT 0 CHECK (copay_percent >= 0 AND copay_percent <= 100),
    copay_max_per_claim  numeric(15,2) CHECK (copay_max_per_claim >= 0),
    deductible_per_claim numeric(15,2) NOT NULL DEFAULT 0 CHECK (deductible_per_claim >= 0),
    visit_limit          numeric(15,2) CHECK (visit_limit > 0),
    UNIQUE (plan_id, care_type)
);
ALTER TABLE core.payer_plan_rule ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_plan_rule_isolation ON core.payer_plan_rule USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_plan_rule_tenant_check BEFORE INSERT OR UPDATE ON core.payer_plan_rule FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.payer_plan_limit (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    plan_id         uuid NOT NULL REFERENCES core.payer_plan(id) ON DELETE CASCADE,
    item_type       text NOT NULL CHECK (item_type IN ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material')),
    limit_per_visit numeric(15,2) NOT NULL CHECK (limit_per_visit > 0),
    UNIQUE (plan_id, item_type)
);
ALTER TABLE core.payer_plan_limit ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_plan_limit_isolation ON core.payer_plan_limit USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_plan_limit_tenant_check BEFORE INSERT OR UPDATE ON core.payer_plan_limit FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.payer_item_coverage (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    payer_id        uuid NOT NULL REFERENCES core.payer(id),
    service_item_id uuid REFERENCES core.service_item(id),
    item_type       text CHECK (item_type IN ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material')),
    coverage        text NOT NULL CHECK (coverage IN ('capitation','claim','excluded','package')),
    claim_type      text CHECK (claim_type IN ('anc','pnc','delivery','kb','prb','ambulance','screening','first_inpatient','pre_referral','other')),
    CONSTRAINT chk_payer_item_coverage_target CHECK ((service_item_id IS NULL) <> (item_type IS NULL)),
    CONSTRAINT chk_payer_item_coverage_claim CHECK ((coverage = 'claim') = (claim_type IS NOT NULL))
);
CREATE UNIQUE INDEX uq_payer_item_coverage_item ON core.payer_item_coverage (payer_id, service_item_id) WHERE service_item_id IS NOT NULL;
CREATE UNIQUE INDEX uq_payer_item_coverage_type ON core.payer_item_coverage (payer_id, item_type) WHERE item_type IS NOT NULL;
ALTER TABLE core.payer_item_coverage ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_item_coverage_isolation ON core.payer_item_coverage USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_item_coverage_tenant_check BEFORE INSERT OR UPDATE ON core.payer_item_coverage FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.patient_coverage (
    id                   uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id           uuid NOT NULL REFERENCES core.company(id),
    merchant_id          uuid NOT NULL REFERENCES core.merchant(id),
    person_id            uuid NOT NULL REFERENCES core.person(id),
    payer_id             uuid NOT NULL REFERENCES core.payer(id),
    plan_id              uuid REFERENCES core.payer_plan(id),
    member_number        text NOT NULL CHECK (btrim(member_number) <> ''),
    bpjs_class           text CHECK (bpjs_class IN ('1','2','3')),
    valid_from           date,
    valid_to             date,
    tpa_remaining_annual numeric(15,2) CHECK (tpa_remaining_annual >= 0),
    tpa_remaining_as_of  date,
    is_active            boolean NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    created_by           uuid,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    updated_by           uuid,
    deleted_at           timestamptz,
    deleted_by           uuid,
    row_version          integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_patient_coverage_tpa CHECK ((tpa_remaining_annual IS NULL) = (tpa_remaining_as_of IS NULL)),
    CONSTRAINT chk_patient_coverage_period CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to >= valid_from),
    CONSTRAINT chk_patient_coverage_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_patient_coverage_member ON core.patient_coverage (merchant_id, payer_id, member_number) WHERE deleted_at IS NULL;
CREATE INDEX idx_patient_coverage_person ON core.patient_coverage (person_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_patient_coverage_touch BEFORE UPDATE ON core.patient_coverage FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.patient_coverage ENABLE ROW LEVEL SECURITY;
CREATE POLICY patient_coverage_isolation ON core.patient_coverage USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_patient_coverage_tenant_check BEFORE INSERT OR UPDATE ON core.patient_coverage FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
CREATE TRIGGER trg_person_not_merged BEFORE INSERT OR UPDATE OF person_id ON core.patient_coverage
    FOR EACH ROW EXECUTE FUNCTION core.trg_person_not_merged();

ALTER TABLE operations.admission_guarantor
    ADD COLUMN coverage_id uuid REFERENCES core.patient_coverage(id),
    ADD COLUMN plan_id uuid REFERENCES core.payer_plan(id);

CREATE TABLE operations.guarantee_letter (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    admission_id uuid NOT NULL REFERENCES operations.admission(id),
    payer_id     uuid NOT NULL REFERENCES core.payer(id),
    gl_number    text NOT NULL CHECK (btrim(gl_number) <> ''),
    amount_limit numeric(15,2) NOT NULL CHECK (amount_limit > 0),
    valid_from   date NOT NULL,
    valid_to     date CHECK (valid_to IS NULL OR valid_to >= valid_from),
    note         text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_guarantee_letter_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_guarantee_letter ON operations.guarantee_letter (admission_id, payer_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_guarantee_letter_touch BEFORE UPDATE ON operations.guarantee_letter FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.guarantee_letter ENABLE ROW LEVEL SECURITY;
CREATE POLICY guarantee_letter_isolation ON operations.guarantee_letter USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_guarantee_letter_tenant_check BEFORE INSERT OR UPDATE ON operations.guarantee_letter FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.billing_account (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    admission_id       uuid NOT NULL UNIQUE REFERENCES operations.admission(id),
    status             text NOT NULL DEFAULT 'open' CHECK (status IN ('open','finalized','cancelled')),
    finalized_at       timestamptz,
    finalized_by       uuid,
    reopened_count     integer NOT NULL DEFAULT 0,
    last_reopen_reason text,
    patient_total      numeric(15,2),
    payer_total        numeric(15,2),
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid,
    row_version        integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_billing_account_final CHECK ((status = 'finalized') = (finalized_at IS NOT NULL)
        AND (status = 'finalized') = (patient_total IS NOT NULL AND payer_total IS NOT NULL))
);
CREATE TRIGGER trg_billing_account_touch BEFORE UPDATE ON operations.billing_account FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.billing_account ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_account_isolation ON operations.billing_account USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_billing_account_tenant_check BEFORE INSERT OR UPDATE ON operations.billing_account FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.charge (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    account_id      uuid NOT NULL REFERENCES operations.billing_account(id),
    admission_id    uuid NOT NULL REFERENCES operations.admission(id),
    service_item_id uuid NOT NULL REFERENCES core.service_item(id),
    quantity        numeric(12,3) NOT NULL CHECK (quantity > 0),
    service_date    date NOT NULL,
    source          text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','clinical','dispense')),
    source_ref      uuid,
    note            text,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','void')),
    void_reason     text,
    voided_by       uuid,
    voided_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid,
    row_version     integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_charge_void CHECK ((status = 'void') = (voided_at IS NOT NULL AND btrim(coalesce(void_reason, '')) <> ''))
);
CREATE INDEX idx_charge_account ON operations.charge (account_id, service_date, created_at);
CREATE TRIGGER trg_charge_touch BEFORE UPDATE ON operations.charge FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.charge ENABLE ROW LEVEL SECURITY;
CREATE POLICY charge_isolation ON operations.charge USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_charge_tenant_check BEFORE INSERT OR UPDATE ON operations.charge FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.charge_split (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    charge_id       uuid NOT NULL REFERENCES operations.charge(id) ON DELETE CASCADE,
    seq             smallint NOT NULL,
    party           text NOT NULL CHECK (party IN ('payer','patient')),
    payer_id        uuid REFERENCES core.payer(id),
    coverage        text NOT NULL CHECK (coverage IN ('capitation','claim','package','plan','excluded','uncovered')),
    claim_type      text,
    price_list_id   uuid NOT NULL REFERENCES core.price_list(id),
    unit_amount     numeric(15,2) NOT NULL,
    gross_amount    numeric(15,2) NOT NULL,
    discount_amount numeric(15,2) NOT NULL DEFAULT 0,
    amount          numeric(15,2) NOT NULL CHECK (amount >= 0),
    shadow_amount   numeric(15,2),
    note            text,
    computed_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (charge_id, seq),
    CONSTRAINT chk_charge_split_party CHECK ((party = 'payer') = (payer_id IS NOT NULL))
);
ALTER TABLE operations.charge_split ENABLE ROW LEVEL SECURITY;
CREATE POLICY charge_split_isolation ON operations.charge_split USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_charge_split_tenant_check BEFORE INSERT OR UPDATE ON operations.charge_split FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.billing_consent (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    admission_id      uuid NOT NULL REFERENCES operations.admission(id),
    charge_id         uuid REFERENCES operations.charge(id),
    kind              text NOT NULL DEFAULT 'bpjs_excluded' CHECK (kind IN ('bpjs_excluded')),
    consented_by_name text NOT NULL CHECK (btrim(consented_by_name) <> ''),
    relation          text NOT NULL CHECK (relation IN ('self','family','guardian')),
    recorded_by       uuid NOT NULL,
    recorded_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_billing_consent_admission ON operations.billing_consent (admission_id);
ALTER TABLE operations.billing_consent ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_consent_isolation ON operations.billing_consent USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_billing_consent_tenant_check BEFORE INSERT OR UPDATE ON operations.billing_consent FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON operations.billing_consent FROM app_runtime;

-- §3.14 permission (id pattern of 000043; 035 taken by 000067)
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000036', 'billing.account.reopen', 'Buka ulang tagihan yang sudah final', 'billing')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r JOIN core.permission p ON p.code = 'billing.account.reopen'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'billing.account.reopen'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

-- person merge: re-apply the 000065 function bodies extended with
-- core.patient_coverage (counts/move/unmerge), so RM merge follows coverage.
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
        'core.patient_coverage', (SELECT count(*) FROM core.patient_coverage WHERE person_id = p_merged),
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
    WITH u AS (UPDATE core.patient_coverage SET person_id = p_survivor WHERE person_id = p_merged RETURNING id)
    SELECT COALESCE(array_agg(id), '{}') INTO v_ids FROM u;
    v_moved := v_moved || jsonb_build_object('core.patient_coverage', to_jsonb(v_ids));
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
    v_ids := ARRAY(SELECT jsonb_array_elements_text(COALESCE(v_log.moved->'core.patient_coverage', '[]'))::uuid);
    UPDATE core.patient_coverage SET person_id = v_m WHERE id = ANY(v_ids) AND person_id = v_s;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_out := v_out || jsonb_build_object('core.patient_coverage', jsonb_build_object('restored', v_n, 'skipped', cardinality(v_ids) - v_n));
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
