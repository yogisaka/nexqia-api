BEGIN;

-- EMPI G2 Task 1: company_name untuk ListEmpiLinksByMaster tidak bisa JOIN
-- core.company langsung — company ber-RLS company isolation (000003:23-25),
-- sehingga di bawah GUC app.current_company_id caller, link milik company
-- lain hilang dari hasil resolve (bug ledger G2 task1, 2026-10-22; keputusan
-- user Opsi B). Pola SECURITY DEFINER preseden platform.list_companies
-- (000039:102-110): SQL statis, hanya master id sebagai parameter, hanya
-- SELECT kolom yang diperlukan, urutan l.id ditetapkan di dalam fungsi.
-- Owner fungsi = role migrasi (owner tabel empi/core) → RLS tidak berlaku.
-- sqlc hanya meresolusi RETURNS SETOF <tabel> (konvensi 000021/000039) —
-- tabel shape-only TANPA baris (tidak pernah diisi), akses ditutup.
CREATE TABLE empi.master_link_company (
    company_id    uuid,
    person_id     uuid,
    full_name     text,
    birth_date    date,
    gender        text,
    status        text,
    company_name  text
);
REVOKE ALL ON TABLE empi.master_link_company FROM app_runtime;

CREATE FUNCTION empi.list_links_by_master(p_master_person_id uuid)
RETURNS SETOF empi.master_link_company
LANGUAGE sql SECURITY DEFINER STABLE
SET search_path = empi, core
AS $$
    SELECT l.company_id, l.person_id, l.full_name, l.birth_date,
           l.gender, l.status, c.name
    FROM empi.person_link l
    JOIN core.company c ON c.id = l.company_id
    WHERE l.master_person_id = p_master_person_id
    ORDER BY l.id
$$;

REVOKE ALL ON FUNCTION empi.list_links_by_master(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION empi.list_links_by_master(uuid) TO app_runtime;

COMMIT;
