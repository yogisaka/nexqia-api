-- Fondasi Role Matrix (spec 2026-10-05-role-matrix-rbac-design §3).
-- role_widget: override visibilitas widget per role; ABSEN baris = default
-- permission-driven dari dashboardConfig.ts. role_change_log: riwayat
-- perubahan izin/widget per role beserta alasan (audit_log TIDAK dipakai —
-- 000046 sudah DROP old_value/new_value dan domainnya data pasien).

CREATE TABLE core.role_widget (
    role_id    uuid NOT NULL REFERENCES core.role(id) ON DELETE CASCADE,
    widget_key text NOT NULL,
    visible    boolean NOT NULL,
    PRIMARY KEY (role_id, widget_key)
);

ALTER TABLE core.role_widget ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_widget_isolation ON core.role_widget
    USING (EXISTS (
        SELECT 1 FROM core.role r
        WHERE r.id = role_widget.role_id
          AND r.company_id = current_setting('app.current_company_id')::uuid
    ));

CREATE TABLE core.role_change_log (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    role_id     uuid NOT NULL REFERENCES core.role(id),
    changed_by  uuid NOT NULL REFERENCES core.app_user(id),
    changed_at  timestamptz NOT NULL DEFAULT now(),
    reason      text NOT NULL,
    changes     jsonb NOT NULL
);

CREATE INDEX idx_role_change_log_role ON core.role_change_log (role_id, changed_at DESC);

ALTER TABLE core.role_change_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_change_log_isolation ON core.role_change_log
    USING (company_id = current_setting('app.current_company_id')::uuid);
