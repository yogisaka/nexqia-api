BEGIN;

CREATE TABLE core.staff_credential (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    user_id         uuid NOT NULL REFERENCES core.app_user(id),
    credential_kind text NOT NULL CHECK (credential_kind IN ('str', 'sip', 'sipa')),
    license_number  text NOT NULL CHECK (btrim(license_number) <> ''),
    valid_until     date,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid NOT NULL REFERENCES core.app_user(id),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    deleted_by      uuid REFERENCES core.app_user(id),
    row_version     integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_staff_credential_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_staff_credential_merchant ON core.staff_credential (merchant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_staff_credential_user ON core.staff_credential (user_id) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_staff_credential_active
    ON core.staff_credential (merchant_id, user_id, credential_kind) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_staff_credential_touch BEFORE UPDATE ON core.staff_credential
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_staff_credential_tenant_check BEFORE INSERT OR UPDATE ON core.staff_credential
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE core.staff_credential ENABLE ROW LEVEL SECURITY;
CREATE POLICY staff_credential_isolation ON core.staff_credential
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

ALTER TABLE core.email_outbox DROP CONSTRAINT email_outbox_kind_check;
ALTER TABLE core.email_outbox ADD CONSTRAINT email_outbox_kind_check
    CHECK (kind IN ('welcome', 'new_device_login', 'staff_credential_expiry'));
ALTER TABLE core.email_outbox ADD COLUMN dedup_key text;
CREATE UNIQUE INDEX uq_email_outbox_dedup_key ON core.email_outbox (dedup_key) WHERE dedup_key IS NOT NULL;

CREATE FUNCTION core.enqueue_credential_email(p_company_id uuid, p_to text, p_subject text, p_text text, p_html text, p_dedup_key text)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    INSERT INTO core.email_outbox (company_id, kind, to_address, subject, body_text, body_html, dedup_key)
    VALUES (p_company_id, 'staff_credential_expiry', p_to, p_subject, p_text, p_html, p_dedup_key)
    ON CONFLICT DO NOTHING;
$$;

CREATE FUNCTION core.credential_merchant_timezone(p_merchant_id uuid)
RETURNS text
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    SELECT m.timezone FROM core.merchant m WHERE m.id = p_merchant_id;
$$;

CREATE FUNCTION core.credential_admin_emails(p_merchant_id uuid)
RETURNS TABLE (id uuid, email text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    SELECT DISTINCT u.id, u.email
    FROM core.app_user u
    JOIN core.user_merchant_role umr ON umr.user_id = u.id AND umr.merchant_id = p_merchant_id
    JOIN core.role_permission rp ON rp.role_id = umr.role_id
    JOIN core.permission pm ON pm.id = rp.permission_id AND pm.code = 'core.user.manage'
    WHERE u.deleted_at IS NULL AND u.is_active AND u.email IS NOT NULL;
$$;

CREATE FUNCTION core.scan_credential_expiry(p_deadline date)
RETURNS TABLE (merchant_id uuid, company_id uuid, staff_kind text, user_id uuid, user_email text,
               staff_name text, credential_kind text, license_number text, valid_until date)
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    SELECT p.merchant_id, p.company_id, 'physician'::text,
           au.id, au.email,
           COALESCE(pe.full_name, au.username, ''),
           v.credential_kind, v.license_number, v.valid_until
    FROM core.physician p
    JOIN core.person pe ON pe.id = p.person_id
    LEFT JOIN LATERAL (
        SELECT au2.id, au2.email, au2.username FROM core.app_user au2
        WHERE au2.person_id = p.person_id AND au2.deleted_at IS NULL AND au2.is_active
        ORDER BY au2.created_at
        LIMIT 1
    ) au ON true
    CROSS JOIN LATERAL (VALUES
        ('str', p.str_number, NULL::date),
        ('sip', p.sip_number, p.sip_valid_until)
    ) AS v(credential_kind, license_number, valid_until)
    WHERE p.deleted_at IS NULL AND v.license_number IS NOT NULL
      AND v.valid_until IS NOT NULL AND v.valid_until <= p_deadline
    UNION ALL
    SELECT sc.merchant_id, sc.company_id, 'staff'::text,
           u.id, u.email,
           COALESCE(pe2.full_name, u.username, ''),
           sc.credential_kind, sc.license_number, sc.valid_until
    FROM core.staff_credential sc
    JOIN core.app_user u ON u.id = sc.user_id AND u.deleted_at IS NULL
    LEFT JOIN core.person pe2 ON pe2.id = u.person_id
    WHERE sc.deleted_at IS NULL AND sc.valid_until IS NOT NULL AND sc.valid_until <= p_deadline;
$$;

REVOKE ALL ON FUNCTION core.enqueue_credential_email(uuid, text, text, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.credential_merchant_timezone(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.credential_admin_emails(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.scan_credential_expiry(date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.enqueue_credential_email(uuid, text, text, text, text, text) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.credential_merchant_timezone(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.credential_admin_emails(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.scan_credential_expiry(date) TO app_runtime;

COMMIT;
