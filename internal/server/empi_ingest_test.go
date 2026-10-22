//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// EMPI ingest tests (plan G1 Task 3, spec §6.1): dua company A/B, person dibuat
// INSERT langsung di bawah GUC company masing-masing; nik_search_hash diisi
// string hex SINTETIS — TANPA NIK plaintext. Assert hanya UUID + hitungan + status.

const (
	empiHashH1 = "aa11bb22cc33dd44ee55ff6677889900"           // hex sintetis, bukan NIK
	empiHashH2 = "ff00112233445566778899aabbccddeeff00112233" // hex sintetis, bukan NIK
)

func empiSeedCompany(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, code string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, max_concurrent_sessions) VALUES ($1, $2, $3, 10)", id, code, "EMPI "+code); err != nil {
		t.Fatalf("seed company %s: %v", code, err)
	}
	return id
}

// empiInsertPerson inserts a person directly under the company GUC and returns its id.
func empiInsertPerson(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, fullName, nikHash string, birthDate string) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set guc: %v", err)
	}
	var id string
	if birthDate != "" {
		err = tx.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, birth_date, nik_search_hash) VALUES ($1, $2, 'female', $3, NULLIF($4, '')) RETURNING id::text",
			companyID, fullName, birthDate, nikHash).Scan(&id)
	} else {
		err = tx.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, nik_search_hash) VALUES ($1, $2, 'female', NULLIF($3, '')) RETURNING id::text",
			companyID, fullName, nikHash).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert person: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// empiIngest runs EmpiIngestPerson in one tx under the company GUC, then commits.
func empiIngest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, personID string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set guc: %v", err)
	}
	if err := server.EmpiIngestPerson(ctx, tx, empiUUID(t, companyID), empiUUID(t, personID)); err != nil {
		t.Fatalf("EmpiIngestPerson: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func empiUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("bad uuid %q: %v", s, err)
	}
	return u
}

func empiCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func empiSeedMaster(t *testing.T, ctx context.Context, pool *pgxpool.Pool, masterID, fullName string, birthDate *string) {
	t.Helper()
	var bd any
	if birthDate != nil {
		bd = *birthDate
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO empi.master_person (id, full_name, birth_date, gender, full_name_source_person_id, birth_date_source_person_id, gender_source_person_id) VALUES ($1, $2, $3, 'female', $4, $4, $4)",
		masterID, fullName, bd, "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("seed master: %v", err)
	}
}

func empiSeedLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, linkID, masterID, companyID, personID, nikHash, fullName string, birthDate *string) {
	t.Helper()
	var bd any
	if birthDate != nil {
		bd = *birthDate
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO empi.person_link (id, master_person_id, company_id, person_id, nik_search_hash, full_name, birth_date, gender, status) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, 'female', 'active')",
		linkID, masterID, companyID, personID, nikHash, fullName, bd); err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

func TestEmpiIngest_AutoLinksSameNikCrossCompany(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-0000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-0000-0000-0000-000000000002", "empi-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "1990-01-01")
	empiIngest(t, ctx, pool, companyA, a1)
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.master_person"); got != 1 {
		t.Fatalf("after A1 ingest: expected 1 master, got %d", got)
	}

	b1 := empiInsertPerson(t, ctx, pool, companyB, "B Satu", empiHashH1, "1990-01-01")
	empiIngest(t, ctx, pool, companyB, b1)

	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.master_person"); got != 1 {
		t.Errorf("after B1 ingest: expected still 1 master, got %d", got)
	}
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.person_link WHERE status='active'"); got != 2 {
		t.Errorf("expected 2 active links, got %d", got)
	}
	var sameMaster int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM empi.person_link l1 JOIN empi.person_link l2 ON l1.master_person_id = l2.master_person_id WHERE l1.person_id = $1 AND l2.person_id = $2",
		a1, b1).Scan(&sameMaster); err != nil {
		t.Fatalf("join links: %v", err)
	}
	if sameMaster != 1 {
		t.Errorf("expected A1 and B1 on the same master, got %d", sameMaster)
	}
}

func TestEmpiIngest_NoNikSeparateMasters(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-1000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-1000-0000-0000-000000000002", "empi-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "")
	empiIngest(t, ctx, pool, companyA, a1)
	b1 := empiInsertPerson(t, ctx, pool, companyB, "B Satu", "", "") // tanpa hash
	empiIngest(t, ctx, pool, companyB, b1)

	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.master_person"); got != 2 {
		t.Errorf("expected 2 separate masters, got %d", got)
	}
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.master_person m WHERE (SELECT count(*) FROM empi.person_link l WHERE l.master_person_id = m.id AND l.status='active') = 1"); got != 2 {
		t.Errorf("expected both masters to have exactly 1 active link, got %d", got)
	}
}

func TestEmpiIngest_NikAddedLaterMovesOrphanMaster(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-2000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-2000-0000-0000-000000000002", "empi-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "")
	empiIngest(t, ctx, pool, companyA, a1)
	var masterA string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", a1).Scan(&masterA); err != nil {
		t.Fatalf("master A1: %v", err)
	}

	b1 := empiInsertPerson(t, ctx, pool, companyB, "B Satu", "", "")
	empiIngest(t, ctx, pool, companyB, b1)
	var masterB string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", b1).Scan(&masterB); err != nil {
		t.Fatalf("master B1: %v", err)
	}
	if masterB == masterA {
		t.Fatalf("precondition: expected B1 on its own master before NIK added")
	}

	// NIK ditambah belakangan: UPDATE hash lalu ingest ulang.
	if _, err := pool.Exec(ctx, "UPDATE core.person SET nik_search_hash = $1 WHERE id = $2", empiHashH1, b1); err != nil {
		t.Fatalf("update hash: %v", err)
	}
	empiIngest(t, ctx, pool, companyB, b1)

	var newMasterB string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", b1).Scan(&newMasterB); err != nil {
		t.Fatalf("master B1 after: %v", err)
	}
	if newMasterB != masterA {
		t.Errorf("expected B1 link moved to M1 (%s), got %s", masterA, newMasterB)
	}
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.person_link WHERE master_person_id = $1 AND status='active'", masterB); got != 0 {
		t.Errorf("expected old master M2 orphan (0 active links), got %d", got)
	}
}

func TestEmpiIngest_SameNikTwoMastersCreatesCandidate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-3000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-3000-0000-0000-000000000002", "empi-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "")
	b2 := empiInsertPerson(t, ctx, pool, companyB, "B Dua", "", "")
	b3 := empiInsertPerson(t, ctx, pool, companyB, "B Tiga", "", "")

	// Seed langsung: M1 + link A1 (hash h1); M2 + link B2 dan B3 (tanpa hash).
	masterM1 := "eeeeeeee-3000-0000-0000-0000000000a1"
	masterM2 := "eeeeeeee-3000-0000-0000-0000000000b2"
	empiSeedMaster(t, ctx, pool, masterM1, "A Satu", nil)
	empiSeedMaster(t, ctx, pool, masterM2, "B Dua", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-3000-0000-0000-000000000001", masterM1, companyA, a1, empiHashH1, "A Satu", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-3000-0000-0000-000000000002", masterM2, companyB, b2, "", "B Dua", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-3000-0000-0000-000000000003", masterM2, companyB, b3, "", "B Tiga", nil)

	if _, err := pool.Exec(ctx, "UPDATE core.person SET nik_search_hash = $1 WHERE id = $2", empiHashH1, b2); err != nil {
		t.Fatalf("update hash B2: %v", err)
	}
	empiIngest(t, ctx, pool, companyB, b2)

	// B2 tetap di M2 (M2 punya link aktif lain = B3).
	var masterB2 string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", b2).Scan(&masterB2); err != nil {
		t.Fatalf("master B2: %v", err)
	}
	if masterB2 != masterM2 {
		t.Errorf("expected B2 to stay on M2 (%s), got %s", masterM2, masterB2)
	}
	var a1Master string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", a1).Scan(&a1Master); err != nil {
		t.Fatalf("master A1: %v", err)
	}
	if a1Master != masterM1 {
		t.Errorf("no master ownership change expected: A1 moved from M1 to %s", a1Master)
	}
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE reason='same_nik' AND status='pending' AND score=100"); got != 1 {
		t.Fatalf("expected exactly 1 pending same_nik pair, got %d", got)
	}
	var aSide, bSide, aCompany, bCompany string
	if err := pool.QueryRow(ctx,
		"SELECT person_a_id::text, person_b_id::text, person_a_company_id::text, person_b_company_id::text FROM empi.candidate_pair WHERE reason='same_nik'").
		Scan(&aSide, &bSide, &aCompany, &bCompany); err != nil {
		t.Fatalf("read pair: %v", err)
	}
	if !((aSide == b2 && bSide == a1) || (aSide == a1 && bSide == b2)) {
		t.Errorf("expected pair (B2, A1), got (%s, %s)", aSide, bSide)
	}
	if !((aCompany == companyA && bCompany == companyB) || (aCompany == companyB && bCompany == companyA)) {
		t.Errorf("expected companies (A, B), got (%s, %s)", aCompany, bCompany)
	}
}

func TestEmpiIngest_SameNikReopensDismissedPair(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-4000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-4000-0000-0000-000000000002", "empi-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "")
	b2 := empiInsertPerson(t, ctx, pool, companyB, "B Dua", "", "")
	b3 := empiInsertPerson(t, ctx, pool, companyB, "B Tiga", "", "")
	masterM1 := "eeeeeeee-4000-0000-0000-0000000000a1"
	masterM2 := "eeeeeeee-4000-0000-0000-0000000000b2"
	empiSeedMaster(t, ctx, pool, masterM1, "A Satu", nil)
	empiSeedMaster(t, ctx, pool, masterM2, "B Dua", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-4000-0000-0000-000000000001", masterM1, companyA, a1, empiHashH1, "A Satu", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-4000-0000-0000-000000000002", masterM2, companyB, b2, "", "B Dua", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-4000-0000-0000-000000000003", masterM2, companyB, b3, "", "B Tiga", nil)

	if _, err := pool.Exec(ctx, "UPDATE core.person SET nik_search_hash = $1 WHERE id = $2", empiHashH1, b2); err != nil {
		t.Fatalf("update hash B2: %v", err)
	}
	empiIngest(t, ctx, pool, companyB, b2)

	// Operator dismiss pasangan (UPDATE langsung, decided_by/decided_at terisi).
	decidedBy := "eeeeeeee-4000-0000-0000-0000000000d1"
	if _, err := pool.Exec(ctx,
		"UPDATE empi.candidate_pair SET status='dismissed', decided_by=$1, decided_at=now() WHERE reason='same_nik'", decidedBy); err != nil {
		t.Fatalf("dismiss pair: %v", err)
	}
	var status string
	var decidedByGot *string
	if err := pool.QueryRow(ctx,
		"SELECT status, decided_by::text FROM empi.candidate_pair WHERE reason='same_nik'").Scan(&status, &decidedByGot); err != nil {
		t.Fatalf("read pair: %v", err)
	}
	if status != "dismissed" {
		t.Fatalf("precondition: expected dismissed, got %s", status)
	}

	// Ingest ulang hash sama → re-open: pending + decided_* NULL.
	empiIngest(t, ctx, pool, companyB, b2)
	if err := pool.QueryRow(ctx,
		"SELECT status, decided_by::text, decided_at FROM empi.candidate_pair WHERE reason='same_nik'").Scan(&status, &decidedByGot, new(any)); err != nil {
		t.Fatalf("read pair after re-ingest: %v", err)
	}
	if status != "pending" {
		t.Errorf("expected pair re-opened to pending, got %s", status)
	}
	if decidedByGot != nil {
		t.Errorf("expected decided_by NULL after re-open, got %s", *decidedByGot)
	}
}

func TestEmpiIngest_AutoLinkTwoMatchingMastersCreatesCandidate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-8000-0000-0000-000000000001", "empi-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-8000-0000-0000-000000000002", "empi-b")
	companyC := empiSeedCompany(t, ctx, pool, "eeeeeeee-8000-0000-0000-000000000003", "empi-c")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "")
	b2 := empiInsertPerson(t, ctx, pool, companyB, "B Dua", empiHashH1, "")
	c3 := empiInsertPerson(t, ctx, pool, companyC, "C Tiga", empiHashH1, "")

	// Dua master ber-hash sama (seed langsung): M1 (id lebih kecil) + link A1,
	// M2 + link B2 — tidak ada link untuk C3.
	masterM1 := "10000000-8000-0000-0000-0000000000a1"
	masterM2 := "20000000-8000-0000-0000-0000000000b2"
	empiSeedMaster(t, ctx, pool, masterM1, "A Satu", nil)
	empiSeedMaster(t, ctx, pool, masterM2, "B Dua", nil)
	empiSeedLink(t, ctx, pool, "10000000-8000-0000-0000-000000000001", masterM1, companyA, a1, empiHashH1, "A Satu", nil)
	empiSeedLink(t, ctx, pool, "20000000-8000-0000-0000-000000000002", masterM2, companyB, b2, empiHashH1, "B Dua", nil)

	empiIngest(t, ctx, pool, companyC, c3)

	// Auto-link deterministik: C3 → targets[0] = M1 (id terkecil).
	var masterC3 string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", c3).Scan(&masterC3); err != nil {
		t.Fatalf("master C3: %v", err)
	}
	if masterC3 != masterM1 {
		t.Errorf("expected C3 auto-linked to smallest master M1 (%s), got %s", masterM1, masterC3)
	}
	// Kandidat same_nik utk targets[1..]: C3 vs B2 (master M2), pending skor 100.
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE reason='same_nik' AND status='pending' AND score=100"); got != 1 {
		t.Fatalf("expected exactly 1 pending same_nik pair, got %d", got)
	}
	var aSide, bSide string
	if err := pool.QueryRow(ctx,
		"SELECT person_a_id::text, person_b_id::text FROM empi.candidate_pair WHERE reason='same_nik'").
		Scan(&aSide, &bSide); err != nil {
		t.Fatalf("read pair: %v", err)
	}
	if !((aSide == c3 && bSide == b2) || (aSide == b2 && bSide == c3)) {
		t.Errorf("expected pair (C3, B2), got (%s, %s)", aSide, bSide)
	}
	// Tidak pernah auto-merge: kepemilikan link A1/B2 tak berubah.
	var linksM1, linksM2 int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM empi.person_link WHERE master_person_id = $1 AND status='active'", masterM1).Scan(&linksM1); err != nil {
		t.Fatalf("count M1: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM empi.person_link WHERE master_person_id = $1 AND status='active'", masterM2).Scan(&linksM2); err != nil {
		t.Fatalf("count M2: %v", err)
	}
	if linksM1 != 2 || linksM2 != 1 { // M1: A1+C3; M2: B2 tetap
		t.Errorf("expected M1=2 links, M2=1 link, got M1=%d M2=%d", linksM1, linksM2)
	}
}

func TestEmpiIngest_SoftDeleteDetachesLink(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-5000-0000-0000-000000000001", "empi-a")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "A Satu", empiHashH1, "1990-01-01")
	empiIngest(t, ctx, pool, companyA, a1)
	var masterA string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", a1).Scan(&masterA); err != nil {
		t.Fatalf("master A1: %v", err)
	}

	deletedBy := "eeeeeeee-5000-0000-0000-0000000000d1"
	if _, err := pool.Exec(ctx, "UPDATE core.person SET deleted_at = now(), deleted_by = $1 WHERE id = $2", deletedBy, a1); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	empiIngest(t, ctx, pool, companyA, a1)

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM empi.person_link WHERE person_id = $1", a1).Scan(&status); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if status != "detached" {
		t.Errorf("expected link detached, got %s", status)
	}
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.master_person WHERE id = $1", masterA); got != 1 {
		t.Errorf("expected master to remain, got %d", got)
	}
	// Golden dipertahankan (tidak di-nol-kan saat detach).
	var fullName string
	if err := pool.QueryRow(ctx, "SELECT full_name FROM empi.master_person WHERE id = $1", masterA).Scan(&fullName); err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if fullName != "A Satu" {
		t.Errorf("expected golden full_name preserved, got %q", fullName)
	}
}

func TestEmpiIngest_RecomputeGoldenFirstNonEmpty(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-6000-0000-0000-000000000001", "empi-a")

	// Link pertama (id lebih kecil): full_name='', birth_date=D1.
	// Link kedua: full_name='Siti', birth_date=D2.
	d1, d2 := "1990-01-01", "1991-02-02"
	p1 := empiInsertPerson(t, ctx, pool, companyA, "P Satu", "", d1)
	p2 := empiInsertPerson(t, ctx, pool, companyA, "Siti", "", d2)
	master := "eeeeeeee-6000-0000-0000-0000000000aa"
	empiSeedMaster(t, ctx, pool, master, "", &d1)
	empiSeedLink(t, ctx, pool, "10000000-0000-0000-0000-000000000001", master, companyA, p1, "", "", &d1)
	empiSeedLink(t, ctx, pool, "20000000-0000-0000-0000-000000000002", master, companyA, p2, "", "Siti", &d2)

	empiIngest(t, ctx, pool, companyA, p2)

	var fullName, nameSrc, birthSrc string
	var birthDate time.Time
	if err := pool.QueryRow(ctx,
		"SELECT full_name, full_name_source_person_id::text, birth_date, birth_date_source_person_id::text FROM empi.master_person WHERE id = $1",
		master).Scan(&fullName, &nameSrc, &birthDate, &birthSrc); err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if fullName != "Siti" {
		t.Errorf("expected golden full_name 'Siti' (first non-empty), got %q", fullName)
	}
	if nameSrc != p2 {
		t.Errorf("expected full_name_source = person link2 (%s), got %s", p2, nameSrc)
	}
	if got := birthDate.Format("2006-01-02"); got != d1 {
		t.Errorf("expected golden birth_date %s (first non-null), got %s", d1, got)
	}
	if birthSrc != p1 {
		t.Errorf("expected birth_date_source = person link1 (%s), got %s", p1, birthSrc)
	}
}

func TestEmpiIngest_PersonNotFoundFails(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-7000-0000-0000-000000000001", "empi-a")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyA); err != nil {
		t.Fatalf("set guc: %v", err)
	}
	err = server.EmpiIngestPerson(ctx, tx, empiUUID(t, companyA), empiUUID(t, "99999999-9999-9999-9999-999999999999"))
	if err == nil {
		t.Fatal("expected error for unknown person, got nil")
	}
	if !strings.HasPrefix(err.Error(), "empi ingest:") {
		t.Errorf("expected error prefixed 'empi ingest:', got %q", err.Error())
	}
}
