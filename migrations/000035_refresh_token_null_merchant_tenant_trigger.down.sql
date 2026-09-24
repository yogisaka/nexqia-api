-- Restore original strict trigger (000034 schema still allows NULL merchant_id,
-- but this trigger would reject NULL inserts again).
CREATE OR REPLACE FUNCTION trg_check_tenant_consistency()
RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM core.merchant m
        WHERE m.id = NEW.merchant_id AND m.company_id = NEW.company_id
    ) THEN
        RAISE EXCEPTION 'tenant mismatch: merchant_id % bukan milik company_id %', NEW.merchant_id, NEW.company_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
