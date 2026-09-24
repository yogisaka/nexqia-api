-- Follow-up to 000034: the tenant-consistency trigger (000001) must tolerate a
-- NULL merchant_id — session.Issue now legitimately creates refresh tokens for
-- a just-registered Owner who has no merchant yet. NULL merchant means
-- "company-level session, no merchant scope", which is valid; only a
-- NON-NULL merchant_id that doesn't belong to the row's company is a violation.
CREATE OR REPLACE FUNCTION trg_check_tenant_consistency()
RETURNS trigger AS $$
BEGIN
    IF NEW.merchant_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM core.merchant m
        WHERE m.id = NEW.merchant_id AND m.company_id = NEW.company_id
    ) THEN
        RAISE EXCEPTION 'tenant mismatch: merchant_id % bukan milik company_id %', NEW.merchant_id, NEW.company_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
