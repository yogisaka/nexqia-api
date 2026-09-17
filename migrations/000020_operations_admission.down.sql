-- migrations/000020_operations_admission.down.sql
ALTER TABLE operations.queue DROP CONSTRAINT fk_queue_admission;
DROP TABLE operations.admission;
