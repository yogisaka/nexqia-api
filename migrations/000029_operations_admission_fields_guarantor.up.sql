-- migrations/000029_operations_admission_fields_guarantor.up.sql
-- Verbatim transcript of the pendaftaran addendum §2: 3 rawat-jalan columns on
-- operations.admission + the admission_guarantor satellite table (deferred in
-- 000020).

ALTER TABLE operations.admission ADD COLUMN complaint text;
ALTER TABLE operations.admission ADD COLUMN referral_source text;
ALTER TABLE operations.admission ADD COLUMN note text;

CREATE TABLE operations.admission_guarantor (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    admission_id  uuid NOT NULL REFERENCES operations.admission(id),
    payer_id      uuid NOT NULL REFERENCES core.payer(id),
    policy_number text,
    guarantor_name text,
    sequence      smallint NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    UNIQUE (admission_id, sequence)
);
CREATE INDEX idx_admission_guarantor_admission ON operations.admission_guarantor (admission_id);
