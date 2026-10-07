-- nexqia-api/migrations/000063_price_lists.up.sql
-- Spec 2026-10-07-tariff-price-lists §3/§6: layered price lists (base /
-- derived / standalone) replacing service_rate.payer_class, per-category
-- adjustments, ICD-9-CM link on service_item, and the platform price
-- template with BPJS non-capitation tariffs (Permenkes 3/2023).
-- payer_class is kept here and dropped by 000064 once the API stops using it.

ALTER TABLE core.service_item DROP CONSTRAINT service_item_item_type_check;
ALTER TABLE core.service_item ADD CONSTRAINT service_item_item_type_check CHECK (item_type IN
    ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material'));
ALTER TABLE core.service_item ADD COLUMN procedure_concept_id uuid REFERENCES terminology.concept(id);

CREATE TABLE core.price_list (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    code               text NOT NULL CHECK (btrim(code) <> ''),
    name               text NOT NULL CHECK (btrim(name) <> ''),
    base_price_list_id uuid REFERENCES core.price_list(id),
    adjustment_percent numeric(7,2) NOT NULL DEFAULT 0
        CHECK (adjustment_percent > -100 AND adjustment_percent <= 1000),
    rounding_unit      numeric(15,2) NOT NULL DEFAULT 0 CHECK (rounding_unit >= 0),
    is_active          boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid,
    deleted_at         timestamptz,
    deleted_by         uuid,
    row_version        integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_price_list_not_self CHECK (base_price_list_id IS NULL OR base_price_list_id <> id),
    CONSTRAINT chk_price_list_base_no_adjustment CHECK (base_price_list_id IS NOT NULL OR (adjustment_percent = 0 AND rounding_unit = 0)),
    CONSTRAINT chk_price_list_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_price_list_merchant ON core.price_list (merchant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_price_list_base ON core.price_list (base_price_list_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_price_list_touch BEFORE UPDATE ON core.price_list
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.price_list ENABLE ROW LEVEL SECURITY;
CREATE POLICY price_list_isolation ON core.price_list
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_price_list_tenant_check BEFORE INSERT OR UPDATE ON core.price_list
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- One level of derivation (spec §3.2). SECURITY DEFINER: the checks must see
-- rows of every merchant, not only the RLS-visible ones.
CREATE FUNCTION core.trg_price_list_one_level() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
BEGIN
    IF NEW.base_price_list_id IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM core.price_list b
                    WHERE b.id = NEW.base_price_list_id
                      AND (b.base_price_list_id IS NOT NULL OR b.merchant_id <> NEW.merchant_id
                           OR b.deleted_at IS NOT NULL)) THEN
            RAISE EXCEPTION 'price list base must be a live base list of the same merchant'
                USING ERRCODE = 'check_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM core.price_list d
                    WHERE d.base_price_list_id = NEW.id AND d.deleted_at IS NULL) THEN
            RAISE EXCEPTION 'price list is the base of other lists and cannot be derived itself'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_price_list_one_level BEFORE INSERT OR UPDATE ON core.price_list
    FOR EACH ROW EXECUTE FUNCTION core.trg_price_list_one_level();

CREATE TABLE core.price_list_adjustment (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    price_list_id      uuid NOT NULL REFERENCES core.price_list(id) ON DELETE CASCADE,
    item_type          text NOT NULL CHECK (item_type IN
        ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material')),
    adjustment_percent numeric(7,2) NOT NULL CHECK (adjustment_percent > -100 AND adjustment_percent <= 1000),
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    UNIQUE (price_list_id, item_type)
);
ALTER TABLE core.price_list_adjustment ENABLE ROW LEVEL SECURITY;
CREATE POLICY price_list_adjustment_isolation ON core.price_list_adjustment
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_price_list_adjustment_tenant_check BEFORE INSERT OR UPDATE ON core.price_list_adjustment
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- service_rate: payer_class → price_list_id (data migrated; payer_class dropped by 000064).
ALTER TABLE core.service_rate ADD COLUMN price_list_id uuid REFERENCES core.price_list(id);
INSERT INTO core.price_list (company_id, merchant_id, code, name)
SELECT DISTINCT ON (r.merchant_id, r.payer_class) r.company_id, r.merchant_id, r.payer_class,
       CASE r.payer_class WHEN 'general' THEN 'Umum' WHEN 'bpjs' THEN 'BPJS' ELSE r.payer_class END
FROM core.service_rate r
ORDER BY r.merchant_id, r.payer_class;
UPDATE core.service_rate r SET price_list_id = pl.id
FROM core.price_list pl
WHERE pl.merchant_id = r.merchant_id AND pl.code = r.payer_class;
ALTER TABLE core.service_rate ALTER COLUMN price_list_id SET NOT NULL;
CREATE INDEX idx_service_rate_list ON core.service_rate
    (merchant_id, price_list_id, service_item_id, effective_from) WHERE deleted_at IS NULL;

-- Platform price templates (spec §3.5/§6).
ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check CHECK (kind IN ('role', 'department', 'price_list'));
ALTER TABLE core.template ADD CONSTRAINT chk_template_price_list_usage CHECK (kind <> 'price_list' OR usage = 'copy');

CREATE TABLE core.template_price_item (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    template_id uuid NOT NULL REFERENCES core.template(id) ON DELETE CASCADE,
    code        text NOT NULL,
    name        text NOT NULL,
    item_type   text NOT NULL CHECK (item_type IN
        ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material')),
    value_kind  text NOT NULL CHECK (value_kind IN ('fixed','range','max','regional')),
    amount_min  numeric(15,2),
    amount_max  numeric(15,2),
    scopes      text[] NOT NULL CHECK (scopes <@ ARRAY['fktp','bidan_jejaring']::text[] AND cardinality(scopes) > 0),
    legal_ref   text NOT NULL,
    note        text,
    sort_order  integer NOT NULL,
    UNIQUE (template_id, code),
    CONSTRAINT chk_template_price_item_amounts CHECK (
        (value_kind = 'fixed'    AND amount_min IS NOT NULL AND amount_min = amount_max) OR
        (value_kind = 'range'    AND amount_min IS NOT NULL AND amount_max IS NOT NULL AND amount_min < amount_max) OR
        (value_kind = 'max'      AND amount_min IS NULL AND amount_max IS NOT NULL) OR
        (value_kind = 'regional' AND amount_min IS NULL AND amount_max IS NULL))
);
ALTER TABLE core.template_price_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_price_item_visibility ON core.template_price_item
    USING (EXISTS (SELECT 1 FROM core.template t WHERE t.id = template_id));
REVOKE INSERT, UPDATE, DELETE ON core.template_price_item FROM app_runtime;

INSERT INTO core.template (kind, code, name, description, scope, usage, version)
VALUES ('price_list', 'bpjs_nonkapitasi_2023', 'Tarif Non-Kapitasi BPJS — Permenkes 3/2023',
        'Tarif klaim non-kapitasi FKTP selain puskesmas dan bidan jejaring (Permenkes 3/2023 Pasal 12–24).',
        'platform', 'copy', 1);

INSERT INTO core.template_price_item (template_id, code, name, item_type, value_kind, amount_min, amount_max, scopes, legal_ref, sort_order)
SELECT t.id, v.code, v.name, v.item_type, v.value_kind, v.amount_min, v.amount_max, v.scopes, v.legal_ref, v.sort_order
FROM core.template t
JOIN (VALUES
    ('ANC-DR-USG', 'ANC oleh dokter disertai USG', 'procedure', 'fixed', 160000::numeric, 160000::numeric, ARRAY['fktp'], 'Permenkes 3/2023 Ps 19 (2) b 1) a)', 10),
    ('ANC-DR', 'ANC oleh dokter', 'procedure', 'fixed', 90000, 90000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 19 (2) b 1) b)', 20),
    ('ANC-BIDAN', 'ANC oleh bidan', 'procedure', 'fixed', 70000, 70000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 19 (2) b 1) c), (2) c', 30),
    ('PRARUJUK-KOMPLIKASI', 'Pra rujukan komplikasi kehamilan', 'procedure', 'max', NULL, 200000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 19 (6)', 40),
    ('PERSALINAN-TIM-DR', 'Persalinan oleh tim dengan dokter', 'procedure', 'fixed', 1200000, 1200000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 20 (1) a, (2) b', 50),
    ('PERSALINAN-TIM-NAKES', 'Persalinan oleh tim paling sedikit 2 nakes tanpa dokter (kondisi tertentu)', 'procedure', 'fixed', 800000, 800000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 20 (1) b, (2) c', 60),
    ('PERSALINAN-PONED-2H', 'Persalinan tindakan emergensi dasar PONED, 2 hari', 'procedure', 'fixed', 1250000, 1250000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 20 (3) a', 70),
    ('PERSALINAN-PONED-3H', 'Persalinan tindakan emergensi dasar PONED, 3 hari', 'procedure', 'fixed', 1500000, 1500000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 20 (3) b', 80),
    ('PASCA-PERSALINAN-PONED', 'Tindakan pasca persalinan PONED', 'procedure', 'fixed', 180000, 180000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 20 (4)', 90),
    ('PNC', 'Kunjungan nifas dan neonatal (per kunjungan)', 'procedure', 'fixed', 50000, 50000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 21 (5) b, c', 100),
    ('KB-AKDR', 'Pemasangan/pencabutan AKDR', 'procedure', 'fixed', 105000, 105000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 22 (2) a', 110),
    ('KB-IMPLAN', 'Pemasangan/pencabutan implan', 'procedure', 'fixed', 105000, 105000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 22 (2) b', 120),
    ('KB-SUNTIK', 'Suntik KB (per suntik)', 'procedure', 'fixed', 20000, 20000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 22 (2) c', 130),
    ('KB-KOMPLIKASI', 'Penanganan komplikasi KB', 'procedure', 'fixed', 125000, 125000, ARRAY['fktp','bidan_jejaring'], 'Permenkes 3/2023 Ps 22 (2) d', 140),
    ('KB-MOP', 'KB metode operasi pria (vasektomi)', 'procedure', 'fixed', 370000, 370000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 22 (2) e', 150),
    ('SKR-IVA', 'Skrining IVA', 'procedure', 'max', NULL, 25000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 15 (4) a', 160),
    ('SKR-PAPSMEAR', 'Skrining Pap smear', 'procedure', 'max', NULL, 125000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 15 (4) b', 170),
    ('SKR-GULA-DARAH', 'Skrining gula darah (GDS/GDP/GDPP, masing-masing)', 'procedure', 'range', 10000, 20000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 15 (4) c', 180),
    ('SKR-THALASSEMIA', 'Darah lengkap dan apus darah tepi (skrining thalassemia)', 'procedure', 'fixed', 55000, 55000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 15 (4) e', 190),
    ('SKR-KANKER-USUS', 'Rectal touche dan darah samar feses', 'procedure', 'fixed', 45000, 45000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 15 (4) f', 200),
    ('KRIO', 'Terapi krio (IVA positif)', 'procedure', 'fixed', 150000, 150000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 16', 210),
    ('RANAP-TP', 'Rawat inap tingkat pertama (per hari)', 'room', 'range', 200000, 300000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 17 (1)', 220),
    ('PRB-GULA-DARAH', 'Gula darah (GDS/GDP/GDPP) program penyakit kronis', 'procedure', 'range', 10000, 20000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) a', 230),
    ('PRB-HBA1C', 'HbA1c program penyakit kronis', 'procedure', 'range', 160000, 200000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) b', 240),
    ('PRB-UREUM', 'Ureum', 'procedure', 'fixed', 30000, 30000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 1)', 250),
    ('PRB-KREATININ', 'Kreatinin', 'procedure', 'fixed', 30000, 30000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 2)', 260),
    ('PRB-KOL-TOTAL', 'Kolesterol total', 'procedure', 'fixed', 45000, 45000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 3)', 270),
    ('PRB-KOL-LDL', 'Kolesterol LDL', 'procedure', 'fixed', 60000, 60000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 4)', 280),
    ('PRB-KOL-HDL', 'Kolesterol HDL', 'procedure', 'fixed', 45000, 45000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 5)', 290),
    ('PRB-TRIGLISERIDA', 'Trigliserida', 'procedure', 'fixed', 50000, 50000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 6)', 300),
    ('PRB-MICROALBUMIN', 'Microalbuminuria', 'procedure', 'fixed', 120000, 120000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 14 (3) c 7)', 310),
    ('PROTESA-2-RAHANG', 'Protesa gigi lengkap 2 rahang', 'procedure', 'max', NULL, 1000000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 24 (2) a', 320),
    ('PROTESA-1-RAHANG', 'Protesa gigi lengkap 1 rahang', 'procedure', 'max', NULL, 500000, ARRAY['fktp'], 'Permenkes 3/2023 Ps 24 (2) b', 330),
    ('AMBULANS', 'Ambulans rujukan', 'procedure', 'regional', NULL, NULL, ARRAY['fktp'], 'Permenkes 3/2023 Ps 12 (2)-(3), tarif Pemda', 340)
) AS v(code, name, item_type, value_kind, amount_min, amount_max, scopes, legal_ref, sort_order) ON true
WHERE t.kind = 'price_list' AND t.code = 'bpjs_nonkapitasi_2023' AND t.scope = 'platform';

-- Seed integrity: the template must hold exactly 34 rows.
DO $$
BEGIN
    IF (SELECT count(*) FROM core.template_price_item i JOIN core.template t ON t.id = i.template_id
        WHERE t.code = 'bpjs_nonkapitasi_2023' AND t.scope = 'platform') <> 34 THEN
        RAISE EXCEPTION 'bpjs_nonkapitasi_2023 template must have 34 items';
    END IF;
END $$;
