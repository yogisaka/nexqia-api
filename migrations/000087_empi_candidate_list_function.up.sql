BEGIN;

-- EMPI G2 review fix (2026-10-22): ListEmpiCandidatePairs JOIN core.company
-- langsung kena RLS company isolation (000003:23-25) — di bawah GUC
-- app.current_company_id caller, pasangan lintas company terbuang dan review
-- queue selalu kosong (temuan review G2 #1). Pola SECURITY DEFINER migrasi
-- 000086 (keputusan user Opsi B): SQL statis, hanya limit/offset sebagai
-- parameter, hanya SELECT kolom yang diperlukan, urutan di dalam fungsi.
-- Owner fungsi = role migrasi (owner tabel empi/core) → RLS tidak berlaku.
-- sqlc hanya meresolusi RETURNS SETOF <tabel> (konvensi 000021/000039) —
-- tabel shape-only TANPA baris (tidak pernah diisi), akses ditutup.
CREATE TABLE empi.candidate_pair_listing (
    id                  uuid,
    score               integer,
    reason              text,
    status              text,
    note                text,
    a_company_id        uuid,
    a_company_name      text NOT NULL,
    a_person_id         uuid,
    a_full_name         text,
    a_birth_date        date,
    a_gender            text,
    a_has_nik           boolean,
    a_master_person_id  uuid,
    b_company_id        uuid,
    b_company_name      text NOT NULL,
    b_person_id         uuid,
    b_full_name         text,
    b_birth_date        date,
    b_gender            text,
    b_has_nik           boolean,
    b_master_person_id  uuid
);
REVOKE ALL ON TABLE empi.candidate_pair_listing FROM app_runtime;

CREATE FUNCTION empi.list_candidate_pairs(p_limit integer, p_offset integer)
RETURNS SETOF empi.candidate_pair_listing
LANGUAGE sql SECURITY DEFINER STABLE
SET search_path = empi, core
AS $$
    SELECT cp.id, cp.score, cp.reason, cp.status, cp.note,
           a.company_id, ca.name, a.person_id, a.full_name,
           a.birth_date, a.gender, a.nik_search_hash IS NOT NULL,
           a.master_person_id,
           b.company_id, cb.name, b.person_id, b.full_name,
           b.birth_date, b.gender, b.nik_search_hash IS NOT NULL,
           b.master_person_id
    FROM empi.candidate_pair cp
    LEFT JOIN empi.person_link a ON a.person_id = cp.person_a_id AND a.company_id = cp.person_a_company_id
    LEFT JOIN empi.person_link b ON b.person_id = cp.person_b_id AND b.company_id = cp.person_b_company_id
    JOIN core.company ca ON ca.id = cp.person_a_company_id
    JOIN core.company cb ON cb.id = cp.person_b_company_id
    WHERE cp.status = 'pending'
    ORDER BY cp.score DESC, cp.created_at
    LIMIT p_limit OFFSET p_offset
$$;

REVOKE ALL ON FUNCTION empi.list_candidate_pairs(integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION empi.list_candidate_pairs(integer, integer) TO app_runtime;

COMMIT;
