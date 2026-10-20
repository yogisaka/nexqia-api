BEGIN;

DELETE FROM partman.part_config WHERE parent_table = 'connect.integration_request_log';
DROP SCHEMA IF EXISTS connect CASCADE;

COMMIT;
