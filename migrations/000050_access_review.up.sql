-- nexqia-api/migrations/000050_access_review.up.sql
-- #3b-2: evidence that periodic access reviews happened (spec
-- 2026-09-29-audit-log-ui-design §6.3). Append-only for the app role.
CREATE TABLE core.access_review (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    period_from   timestamptz NOT NULL,
    period_to     timestamptz NOT NULL,
    reviewed_by   uuid NOT NULL REFERENCES core.app_user(id),
    reviewed_at   timestamptz NOT NULL DEFAULT now(),
    flagged_users integer NOT NULL CHECK (flagged_users >= 0),
    notes         text NOT NULL CHECK (char_length(notes) BETWEEN 1 AND 2000),
    CHECK (period_to > period_from)
);
CREATE INDEX idx_access_review_company_time ON core.access_review (company_id, reviewed_at);
ALTER TABLE core.access_review ENABLE ROW LEVEL SECURITY;
CREATE POLICY access_review_isolation ON core.access_review
    USING (company_id = current_setting('app.current_company_id')::uuid);
-- Default privileges on schema core grant UPDATE/DELETE to app_runtime (000002); take them back.
REVOKE UPDATE, DELETE ON core.access_review FROM app_runtime;
