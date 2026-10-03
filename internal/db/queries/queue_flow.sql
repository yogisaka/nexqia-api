-- internal/db/queries/queue_flow.sql
-- Configurable queue flows (spec 2026-10-01-c-queue-flow-display-design §3–§4):
-- flow resolution, journeys, and per-stage tickets replacing the hardcoded
-- pendaftaran -> perawat -> dokter pipeline in queue.go.

-- name: ListActiveQueueFlows :many
SELECT * FROM operations.queue_flow
WHERE merchant_id = $1 AND is_active AND deleted_at IS NULL
ORDER BY name;

-- name: GetMerchantCompanyID :one
SELECT company_id FROM core.merchant WHERE id = $1;

-- name: CreateQueueFlowAuto :one
INSERT INTO operations.queue_flow (company_id, merchant_id, name, service_types, department_ids, is_default, is_active)
VALUES ($1, $2, $3, $4, $5, $6, true)
RETURNING *;

-- name: CreateQueueStage :one
INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, served_by_permission)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListQueueStagesByFlow :many
SELECT * FROM operations.queue_stage WHERE flow_id = $1 ORDER BY seq;

-- name: GetQueueStageByID :one
SELECT * FROM operations.queue_stage WHERE id = $1;

-- name: NextQueueStageForFlow :one
SELECT * FROM operations.queue_stage
WHERE flow_id = $1 AND seq > $2
ORDER BY seq
LIMIT 1;

-- name: CreateQueueJourney :one
INSERT INTO operations.queue_journey (company_id, merchant_id, admission_id, person_id, flow_id, current_stage_id, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetQueueJourneyByID :one
SELECT * FROM operations.queue_journey WHERE id = $1;

-- name: GetQueueJourneyByAdmission :one
SELECT * FROM operations.queue_journey WHERE admission_id = $1;

-- name: UpdateQueueJourneyStage :one
UPDATE operations.queue_journey
SET current_stage_id = $2, status = $3, updated_by = $4
WHERE id = $1
RETURNING *;

-- name: CreateQueueFlowTicket :one
INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number, journey_id, stage_id, counter_id, checked_in_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), $11)
RETURNING *;

-- name: CountTicketsForStageSince :one
SELECT count(*) FROM operations.queue
WHERE stage_id = $1 AND created_at >= $2;

-- name: ListCountersForStage :many
SELECT * FROM operations.counter
WHERE stage_id = $1 AND is_active
ORDER BY code;
