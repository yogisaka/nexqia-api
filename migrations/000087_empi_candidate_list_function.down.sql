BEGIN;

DROP FUNCTION IF EXISTS empi.list_candidate_pairs(integer, integer);
DROP TABLE IF EXISTS empi.candidate_pair_listing;

COMMIT;
