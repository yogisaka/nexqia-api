-- migrations/000025_operations_queue_board_view_counter.down.sql
-- Restores operations.queue_board to the 000024 definition (no counter columns).
-- CREATE OR REPLACE VIEW cannot drop columns, so the view must be recreated.
DROP VIEW operations.queue_board;
CREATE VIEW operations.queue_board AS
SELECT
    q.id,
    q.company_id,
    q.merchant_id,
    q.queue_type,
    q.department_id,
    q.queue_number,
    q.status,
    q.called_at,
    q.created_at,
    q.person_id,
    p.full_name AS person_full_name,
    p.medical_record_no AS person_medical_record_no,
    p.birth_date AS person_birth_date,
    q.admission_id,
    a.visit_no AS admission_visit_no,
    a.physician_id,
    phy_person.full_name AS physician_full_name,
    spec.display AS physician_specialty,
    p.photo_url AS person_photo_url,
    phy_person.photo_url AS physician_photo_url
FROM operations.queue q
JOIN core.person p ON p.id = q.person_id
LEFT JOIN operations.admission a ON a.id = q.admission_id
LEFT JOIN core.physician phy ON phy.id = a.physician_id
LEFT JOIN core.person phy_person ON phy_person.id = phy.person_id
LEFT JOIN terminology.concept spec ON spec.id = phy.specialty_concept_id;
