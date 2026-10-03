-- nexqia-api/migrations/000055_core_location.up.sql
-- #21 physical location tree (spec 2026-10-01-a-physical-location-design §3-§4).

CREATE TABLE core.location (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    parent_id     uuid REFERENCES core.location(id),
    kind          text NOT NULL CHECK (kind IN ('site','building','wing','level','room','bed')),
    code          text NOT NULL,
    name          text NOT NULL,
    functions     text[] NOT NULL DEFAULT '{}'
                  CHECK (functions <@ ARRAY['practice','nurse_station','counter','ward_room','treatment','support']::text[]),
    service_class text CHECK (service_class IN ('vip','vvip','1','2','3','kris')),
    capacity      integer CHECK (capacity IS NULL OR capacity > 0),
    latitude      numeric(9,6) CHECK (latitude IS NULL OR latitude BETWEEN -90 AND 90),
    longitude     numeric(9,6) CHECK (longitude IS NULL OR longitude BETWEEN -180 AND 180),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','maintenance','inactive')),
    sort_order    integer NOT NULL DEFAULT 0,
    satusehat_location_id text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    deleted_at    timestamptz,
    deleted_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_location_functions_room CHECK (kind = 'room' OR functions = '{}'),
    CONSTRAINT chk_location_site_position CHECK (kind <> 'site' OR (latitude IS NOT NULL AND longitude IS NOT NULL)),
    CONSTRAINT chk_location_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_location_merchant ON core.location (merchant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_location_parent ON core.location (parent_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_location_touch BEFORE UPDATE ON core.location
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.location ENABLE ROW LEVEL SECURITY;
CREATE POLICY location_isolation ON core.location
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_location_tenant_check BEFORE INSERT OR UPDATE ON core.location
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- Parent rules: same merchant, beds only inside ward rooms, beds have no
-- children, no cycles. Runs as the caller (RLS applies to the lookups).
CREATE OR REPLACE FUNCTION core.trg_location_parent_check() RETURNS trigger AS $$
DECLARE
    p_merchant  uuid;
    p_kind      text;
    p_functions text[];
    cur         uuid;
    depth       integer := 0;
BEGIN
    IF NEW.parent_id IS NULL THEN
        IF NEW.kind = 'bed' THEN
            RAISE EXCEPTION 'bed must be inside a ward room' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    SELECT merchant_id, kind, functions INTO p_merchant, p_kind, p_functions
    FROM core.location WHERE id = NEW.parent_id AND deleted_at IS NULL;
    IF NOT FOUND OR p_merchant <> NEW.merchant_id THEN
        RAISE EXCEPTION 'parent location not found' USING ERRCODE = '23514';
    END IF;
    IF p_kind = 'bed' THEN
        RAISE EXCEPTION 'a bed cannot contain other locations' USING ERRCODE = '23514';
    END IF;
    IF NEW.kind = 'bed' AND NOT (p_kind = 'room' AND 'ward_room' = ANY(p_functions)) THEN
        RAISE EXCEPTION 'bed must be inside a ward room' USING ERRCODE = '23514';
    END IF;
    cur := NEW.parent_id;
    WHILE cur IS NOT NULL LOOP
        IF cur = NEW.id THEN
            RAISE EXCEPTION 'location cannot be its own ancestor' USING ERRCODE = '23514';
        END IF;
        depth := depth + 1;
        IF depth > 20 THEN
            RAISE EXCEPTION 'location tree is too deep' USING ERRCODE = '23514';
        END IF;
        SELECT parent_id INTO cur FROM core.location WHERE id = cur;
    END LOOP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_location_parent_check BEFORE INSERT OR UPDATE OF parent_id, kind, merchant_id ON core.location
    FOR EACH ROW EXECUTE FUNCTION core.trg_location_parent_check();

-- Permission (spec §4). Same id pattern as 000043; 027-029 left unused.
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000030', 'core.location.manage', 'Kelola lokasi fisik: gedung, lantai, ruangan, tempat tidur', 'core')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r JOIN core.permission p ON p.code = 'core.location.manage'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- Platform role templates: admin roles get the new permission; template
-- content changed, so bump the version (role-templates spec §3.3).
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'core.location.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
