BEGIN;

DROP FUNCTION IF EXISTS empi.list_links_by_master(uuid);
DROP TABLE IF EXISTS empi.master_link_company;

COMMIT;
