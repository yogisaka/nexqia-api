-- migrations/000014_core_notification.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §12 "Notification".

CREATE TABLE core.notification (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    channel         text NOT NULL,      -- "antrian","laboratorium","radiologi","cssd", dst
    reference_table text,               -- polymorphic, bukan FK — "admission","lab_order", dst
    reference_id    uuid,
    message         text NOT NULL,
    payload         jsonb,              -- data tambahan buat render/aksi di frontend
    is_read         boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_notification_unread ON core.notification (merchant_id, channel) WHERE NOT is_read;
CREATE TRIGGER trg_notification_touch BEFORE UPDATE ON core.notification
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.notification ENABLE ROW LEVEL SECURITY;
CREATE POLICY notification_isolation ON core.notification
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_notification_tenant_check BEFORE INSERT OR UPDATE ON core.notification
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
