-- internal/db/queries/empi.sql
-- Spec 2026-10-20-empi-cross-company §5-§6, §9. Ingest EMPI: snapshot link,
-- auto-link by nik_search_hash, kandidat same_nik, recompute golden, kursor backfill.

-- name: GetPersonForEmpiIngest :one
-- Dibaca di bawah GUC company pemanggil (RLS utuh); backfill set GUC per batch.
SELECT id, company_id, full_name, birth_date, gender, phone, nik_search_hash, deleted_at
FROM core.person WHERE id = $1;

-- name: LockEmpiNik :exec
-- Advisory lock tx-scoped per hash NIK: dua pendaftaran NIK sama di company
-- berbeda yang bersamaan tidak menciptakan dua master (spec §6.1; pola
-- LockSequence mrn_config.sql). Rilis otomatis saat commit/rollback.
SELECT pg_advisory_xact_lock(hashtext('empi_nik:' || $1::text));

-- name: GetEmpiLinkByPerson :one
SELECT id, master_person_id FROM empi.person_link
WHERE company_id = $1 AND person_id = $2;

-- name: FindEmpiMastersByNikHash :many
-- ORDER BY id terkecil = tie-break deterministik: pemanggil memakai targets[0]
-- untuk auto-link/pindah master (bila ≥2 master ber-hash sama, pilihan stabil).
SELECT DISTINCT master_person_id FROM empi.person_link
WHERE nik_search_hash = $1 AND status = 'active'
ORDER BY master_person_id;

-- name: InsertEmpiMasterPerson :one
INSERT INTO empi.master_person
    (full_name, birth_date, gender, full_name_source_person_id,
     birth_date_source_person_id, gender_source_person_id)
VALUES ($1, $2, $3, $4, $4, $4)
RETURNING id;

-- name: UpsertEmpiPersonLink :one
INSERT INTO empi.person_link
    (master_person_id, company_id, person_id, nik_search_hash,
     full_name, birth_date, gender, phone, status, source_updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', now())
ON CONFLICT (company_id, person_id) DO UPDATE SET
    master_person_id = EXCLUDED.master_person_id,
    nik_search_hash = EXCLUDED.nik_search_hash,
    full_name = EXCLUDED.full_name,
    birth_date = EXCLUDED.birth_date,
    gender = EXCLUDED.gender,
    phone = EXCLUDED.phone,
    status = 'active',
    source_updated_at = now()
RETURNING id, master_person_id;

-- name: DetachEmpiLink :exec
UPDATE empi.person_link SET status = 'detached'
WHERE company_id = $1 AND person_id = $2;

-- name: CountActiveEmpiLinksByMaster :one
SELECT count(*) FROM empi.person_link
WHERE master_person_id = $1 AND status = 'active' AND person_id <> $2;

-- name: GetActiveEmpiLinkByMaster :one
-- Link aktif pertama sebuah master (pasangan kandidat same_nik).
SELECT id, company_id, person_id FROM empi.person_link
WHERE master_person_id = $1 AND status = 'active'
ORDER BY id LIMIT 1;

-- name: UpsertEmpiCandidatePair :one
-- same_nik yang pernah dismissed di-re-open menjadi pending (bukti kuat NIK,
-- spec §6.3/§10); pasangan probabilistik TIDAK di-re-open (pemanggil G2
-- memfilter pasangan dismissed/linked sebelum memanggil).
INSERT INTO empi.candidate_pair
    (person_a_id, person_b_id, person_a_company_id, person_b_company_id, score, reason)
VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid),
        CASE WHEN $1::uuid < $2::uuid THEN $3::uuid ELSE $4::uuid END,
        CASE WHEN $1::uuid < $2::uuid THEN $4::uuid ELSE $3::uuid END,
        $5, $6)
ON CONFLICT (person_a_id, person_b_id) DO UPDATE SET
    score = EXCLUDED.score, reason = EXCLUDED.reason,
    status = CASE WHEN EXCLUDED.reason = 'same_nik' AND empi.candidate_pair.status = 'dismissed'
                  THEN 'pending' ELSE empi.candidate_pair.status END,
    decided_by = CASE WHEN EXCLUDED.reason = 'same_nik' AND empi.candidate_pair.status = 'dismissed'
                      THEN NULL ELSE empi.candidate_pair.decided_by END,
    decided_at = CASE WHEN EXCLUDED.reason = 'same_nik' AND empi.candidate_pair.status = 'dismissed'
                      THEN NULL ELSE empi.candidate_pair.decided_at END
WHERE empi.candidate_pair.status <> 'linked'
RETURNING id, status;

-- name: FindEmpiProbableCandidates :many
WITH n AS (
    SELECT l.* FROM empi.person_link l
    WHERE l.company_id = $1 AND l.person_id = $2 AND l.status = 'active'
)
SELECT o.person_id AS other_person_id, o.company_id AS other_company_id,
       CASE
           WHEN n.birth_date = o.birth_date AND n.gender IS NOT NULL AND n.gender = o.gender
                AND similarity(n.full_name, o.full_name) >= 0.5 THEN 80
           WHEN n.phone IS NOT NULL AND n.phone <> '' AND n.phone = o.phone
                AND similarity(n.full_name, o.full_name) >= 0.5 THEN 70
       END::int AS score,
       CASE
           WHEN n.birth_date = o.birth_date AND n.gender IS NOT NULL AND n.gender = o.gender
                AND similarity(n.full_name, o.full_name) >= 0.5 THEN 'birth_gender_name'
           WHEN n.phone IS NOT NULL AND n.phone <> '' AND n.phone = o.phone
                AND similarity(n.full_name, o.full_name) >= 0.5 THEN 'phone_name'
       END AS reason
FROM n JOIN empi.person_link o
  ON o.status = 'active'
 AND o.company_id <> n.company_id
 AND o.master_person_id <> n.master_person_id
 AND (o.birth_date = n.birth_date
      OR (n.phone IS NOT NULL AND n.phone <> '' AND o.phone = n.phone))
WHERE ((n.birth_date = o.birth_date AND n.gender IS NOT NULL AND n.gender = o.gender
         AND similarity(n.full_name, o.full_name) >= 0.5)
    OR (n.phone IS NOT NULL AND n.phone <> '' AND n.phone = o.phone
        AND similarity(n.full_name, o.full_name) >= 0.5))
   AND NOT EXISTS (
      SELECT 1 FROM empi.candidate_pair cp
      WHERE cp.person_a_id = LEAST(n.person_id, o.person_id)
        AND cp.person_b_id = GREATEST(n.person_id, o.person_id)
        AND cp.status IN ('dismissed','linked')
  )
ORDER BY score DESC, o.person_id;

-- name: RecomputeEmpiMasterGolden :exec
-- Survivorship deterministik (spec §5.1): manual_fields menang; else nilai
-- pertama non-kosong dari link aktif menurut urutan link.id (uuidv7 ≈ urutan
-- ingest; backfill urut created_at). Idempoten: re-run menghasilkan sama.
UPDATE empi.master_person m SET
    full_name = CASE WHEN 'full_name' = ANY(m.manual_fields) THEN m.full_name
        ELSE COALESCE((SELECT l.full_name FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.full_name <> ''
            ORDER BY l.id LIMIT 1), m.full_name) END,
    full_name_source_person_id = CASE WHEN 'full_name' = ANY(m.manual_fields) THEN m.full_name_source_person_id
        ELSE COALESCE((SELECT l.person_id FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.full_name <> ''
            ORDER BY l.id LIMIT 1), m.full_name_source_person_id) END,
    birth_date = CASE WHEN 'birth_date' = ANY(m.manual_fields) THEN m.birth_date
        ELSE COALESCE((SELECT l.birth_date FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.birth_date IS NOT NULL
            ORDER BY l.id LIMIT 1), m.birth_date) END,
    birth_date_source_person_id = CASE WHEN 'birth_date' = ANY(m.manual_fields) THEN m.birth_date_source_person_id
        ELSE COALESCE((SELECT l.person_id FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.birth_date IS NOT NULL
            ORDER BY l.id LIMIT 1), m.birth_date_source_person_id) END,
    gender = CASE WHEN 'gender' = ANY(m.manual_fields) THEN m.gender
        ELSE COALESCE((SELECT l.gender FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.gender IS NOT NULL
            ORDER BY l.id LIMIT 1), m.gender) END,
    gender_source_person_id = CASE WHEN 'gender' = ANY(m.manual_fields) THEN m.gender_source_person_id
        ELSE COALESCE((SELECT l.person_id FROM empi.person_link l
            WHERE l.master_person_id = m.id AND l.status = 'active' AND l.gender IS NOT NULL
            ORDER BY l.id LIMIT 1), m.gender_source_person_id) END
WHERE m.id = $1;

-- name: ListCompaniesForEmpiBackfill :many
-- core.company RLS (000003:23-25) melarang daftar lintas company pada koneksi
-- app_runtime → pakai fungsi SECURITY DEFINER platform (000039:102-110),
-- sudah difilter deleted_at IS NULL.
SELECT id FROM platform.list_companies($1, $2);

-- name: ResolveEmpiLinkByPerson :one
SELECT l.master_person_id, m.full_name, m.birth_date, m.gender,
       m.merged_into_master_person_id
FROM empi.person_link l JOIN empi.master_person m ON m.id = l.master_person_id
WHERE l.company_id = $1 AND l.person_id = $2 AND l.status = 'active';

-- name: ListEmpiLinksByMaster :many
-- company_name lewat fungsi SECURITY DEFINER empi.list_links_by_master
-- (migrasi 000086, keputusan user Opsi B): JOIN core.company langsung kena
-- RLS company (000003:23-25) dan menyembunyikan link lintas company di bawah
-- GUC caller (bug ledger G2 task1). Urutan l.id ditetapkan di dalam fungsi.
SELECT company_id, person_id, full_name, birth_date, gender, status, company_name
FROM empi.list_links_by_master($1);

-- name: GetEmpiMaster :one
SELECT id, full_name, birth_date, gender, merged_into_master_person_id
FROM empi.master_person WHERE id = $1;

-- name: CountEmpiPendingCandidatesByMaster :one
SELECT count(DISTINCT cp.id) FROM empi.candidate_pair cp
JOIN empi.person_link l ON l.person_id IN (cp.person_a_id, cp.person_b_id)
    AND l.company_id IN (cp.person_a_company_id, cp.person_b_company_id)
WHERE l.master_person_id = $1 AND cp.status = 'pending';

-- name: GetPersonMrn :one
SELECT medical_record_no, full_name FROM core.person WHERE id = $1;

-- name: ListPersonsForEmpiBackfill :many
-- Kursor (created_at, id): urutan ini menentukan golden first-non-empty —
-- WAJIB deterministik (spec §9). Dibaca di bawah GUC company batch.
SELECT id FROM core.person
WHERE company_id = $1 AND deleted_at IS NULL
  AND (created_at, id) > ($2, $3::uuid)
ORDER BY created_at, id
LIMIT $4;
