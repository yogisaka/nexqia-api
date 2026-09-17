-- migrations/000019_operations_schedule_counter_queue.down.sql
DROP TABLE operations.queue_status_history;
DROP TABLE operations.queue;
DROP TABLE operations.counter;
DROP TABLE operations.physician_schedule;
