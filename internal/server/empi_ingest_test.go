//go:build integration

package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// empiMergeTwoPersons sets up the shared merge state for the merge/un-merge
// re-ingest tests (request/merge pattern from TestPersonMerge_FullFlow):
// survivor = Beta (tanpa NIK, master sendiri), merged = Alpha (dengan NIK,
// master sendiri); keduanya di-ingest dulu (link active), lalu di-merge via API
// dengan field_choices full_name=merged (survivor mewarisi nama Alpha).
func empiMergeTwoPersons(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerLabel string) (router *gin.Engine, companyID, merchantID, token, survivorID, mergedID string) {
	t.Helper()
	router = mergeTestRouter(t, ctx, pool)
	companyID, merchantID, _, token, _ = seedAccountOwner(t, ctx, pool, router, "empimerge."+ownerLabel, "0812346000"+ownerLabel)

	mergedID, mergedMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge EMPI Alpha", "gender": "female", "birth_date": "1980-01-01",
		"nik": "3201010101800091", "phone": "081111110091",
	})
	survivorID, _ = mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge EMPI Beta", "gender": "female", "birth_date": "1980-01-01",
		"phone": "082222220092",
	})
	empiIngest(t, ctx, pool, companyID, survivorID)
	empiIngest(t, ctx, pool, companyID, mergedID)

	preview := mergePreview(t, router, token, companyID, merchantID, survivorID, mergedID)
	rec := mergeExec(t, router, token, companyID, merchantID, map[string]any{
		"survivor_id": survivorID, "merged_id": mergedID, "reason": "duplikat",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"],
			"merged":   mergeSide(t, preview, "merged")["row_version"],
		},
		"field_choices": map[string]string{"phone": "merged", "full_name": "merged"},
		"confirm_mrn":   mergedMRN,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return router, companyID, merchantID, token, survivorID, mergedID
}

func TestEmpiIngest_MergeDetachesLoser(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyID, merchantID, token, survivorID, mergedID := empiMergeTwoPersons(t, ctx, pool, "011")

	// Loser link detached, master loser (golden) dipertahankan.
	var loserStatus, loserMaster string
	if err := pool.QueryRow(ctx,
		"SELECT status, master_person_id::text FROM empi.person_link WHERE person_id = $1", mergedID,
	).Scan(&loserStatus, &loserMaster); err != nil {
		t.Fatalf("read loser link: %v", err)
	}
	if loserStatus != "detached" {
		t.Errorf("expected loser link detached, got %s", loserStatus)
	}
	var loserGolden string
	if err := pool.QueryRow(ctx,
		"SELECT full_name FROM empi.master_person WHERE id = $1", loserMaster,
	).Scan(&loserGolden); err != nil {
		t.Fatalf("read loser master: %v", err)
	}
	if loserGolden != "Merge EMPI Alpha" {
		t.Errorf("expected loser master golden preserved, got %q", loserGolden)
	}

	// Survivor link active dengan snapshot ter-refresh (full_name mewarisi nama
	// Alpha pilihan merge, persis core.person saat ini).
	var personName, linkName, linkStatus string
	if err := pool.QueryRow(ctx,
		"SELECT p.full_name, l.full_name, l.status FROM core.person p JOIN empi.person_link l ON l.person_id = p.id WHERE p.id = $1",
		survivorID).Scan(&personName, &linkName, &linkStatus); err != nil {
		t.Fatalf("read survivor link: %v", err)
	}
	if linkStatus != "active" {
		t.Errorf("expected survivor link active, got %s", linkStatus)
	}
	if linkName != personName {
		t.Errorf("expected survivor link snapshot refreshed (%q), got %q", personName, linkName)
	}
	if personName != "Merge EMPI Alpha" {
		t.Fatalf("precondition: survivor full_name = %q, want merged Alpha name", personName)
	}

	// merged record tetap 404 (merge tetap berhasil dengan hook EMPI).
	if rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+mergedID, token, companyID, merchantID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET merged person expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEmpiIngest_UnmergeReactivatesBoth(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyID, merchantID, token, survivorID, mergedID := empiMergeTwoPersons(t, ctx, pool, "012")

	// Ambil log id dari list merge-logs person survivor.
	listRec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+survivorID+"/merge-logs", token, companyID, merchantID, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list merge-logs expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	logs := mergeRespList(t, listRec)
	if len(logs) == 0 {
		t.Fatalf("expected merge log rows, got none: %s", listRec.Body.String())
	}
	logID := logs[0].(map[string]any)["id"].(string)

	if rec := mergeUnmerge(t, router, token, companyID, merchantID, logID, "salah gabung"); rec.Code != http.StatusOK {
		t.Fatalf("unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, pid := range []string{survivorID, mergedID} {
		var status string
		if err := pool.QueryRow(ctx,
			"SELECT status FROM empi.person_link WHERE person_id = $1", pid,
		).Scan(&status); err != nil {
			t.Fatalf("read link %s: %v", pid, err)
		}
		if status != "active" {
			t.Errorf("expected link %s active after unmerge, got %s", pid, status)
		}
	}
}

func TestEmpiBackfill_IdempotentConvergent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyC := empiSeedCompany(t, ctx, pool, "eeeeeeee-9000-0000-0000-000000000001", "empi-bf-c")
	companyD := empiSeedCompany(t, ctx, pool, "eeeeeeee-9000-0000-0000-000000000002", "empi-bf-d")

	// Person di company D (hash h1) sudah di-ingest → master M_D ada.
	d1 := empiInsertPerson(t, ctx, pool, companyD, "D Satu", empiHashH1, "1990-01-01")
	empiIngest(t, ctx, pool, companyD, d1)
	var masterD string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", d1).Scan(&masterD); err != nil {
		t.Fatalf("master D1: %v", err)
	}

	// 3 person di company C, INSERT langsung urut created_at eksplisit:
	// p1 hash sama dengan D1 (auto-link ke M_D), p2/p3 tanpa hash (master sendiri).
	p1 := empiInsertPerson(t, ctx, pool, companyC, "C Satu", empiHashH1, "1990-01-01")
	p2 := empiInsertPerson(t, ctx, pool, companyC, "C Dua", "", "1991-02-02")
	p3 := empiInsertPerson(t, ctx, pool, companyC, "C Tiga", "", "1992-03-03")
	for i, pid := range []string{p1, p2, p3} {
		if _, err := pool.Exec(ctx,
			"UPDATE core.person SET created_at = $1 WHERE id = $2",
			time.Date(2026, 1, 2, 10, i, 0, 0, time.UTC), pid); err != nil {
			t.Fatalf("set created_at %s: %v", pid, err)
		}
	}

	empiCounts := func() (masters, links, candidates int64) {
		t.Helper()
		masters = empiCount(t, ctx, pool, "SELECT count(*) FROM empi.master_person")
		links = empiCount(t, ctx, pool, "SELECT count(*) FROM empi.person_link WHERE status='active'")
		candidates = empiCount(t, ctx, pool, "SELECT count(*) FROM empi.candidate_pair")
		return masters, links, candidates
	}

	processed, failed, err := server.BackfillEmpiCompany(ctx, pool, empiUUID(t, companyC), 2)
	if err != nil {
		t.Fatalf("backfill run 1: %v", err)
	}
	if processed != 3 || failed != 0 {
		t.Errorf("run 1: expected processed=3 failed=0, got processed=%d failed=%d", processed, failed)
	}
	m1, l1, c1 := empiCounts()

	processed2, failed2, err := server.BackfillEmpiCompany(ctx, pool, empiUUID(t, companyC), 2)
	if err != nil {
		t.Fatalf("backfill run 2: %v", err)
	}
	if processed2 != 3 || failed2 != 0 {
		t.Errorf("run 2: expected processed=3 failed=0, got processed=%d failed=%d", processed2, failed2)
	}
	m2, l2, c2 := empiCounts()
	if m1 != m2 || l1 != l2 || c1 != c2 {
		t.Errorf("counts diverged between runs: run1=(%d,%d,%d) run2=(%d,%d,%d)", m1, l1, c1, m2, l2, c2)
	}

	// Auto-link deterministik: p1 (hash sama) ke master M_D milik D1.
	var masterP1 string
	if err := pool.QueryRow(ctx, "SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", p1).Scan(&masterP1); err != nil {
		t.Fatalf("master P1: %v", err)
	}
	if masterP1 != masterD {
		t.Errorf("expected P1 auto-linked to M_D (%s), got %s", masterD, masterP1)
	}

	// Golden first-non-empty sesuai urutan (created_at, id): M_D golden dari D1
	// (created lebih awal), master P2/P3 golden dari person masing-masing.
	var goldenD string
	if err := pool.QueryRow(ctx, "SELECT full_name FROM empi.master_person WHERE id = $1", masterD).Scan(&goldenD); err != nil {
		t.Fatalf("read golden M_D: %v", err)
	}
	if goldenD != "D Satu" {
		t.Errorf("expected M_D golden 'D Satu' (first non-empty by created_at), got %q", goldenD)
	}
	for name, pid := range map[string]string{"P2": p2, "P3": p3} {
		var m, golden string
		if err := pool.QueryRow(ctx,
			"SELECT l.master_person_id::text, m.full_name FROM empi.person_link l JOIN empi.master_person m ON m.id = l.master_person_id WHERE l.person_id = $1",
			pid).Scan(&m, &golden); err != nil {
			t.Fatalf("read golden %s: %v", name, err)
		}
		if m == masterD || golden != map[string]string{"P2": "C Dua", "P3": "C Tiga"}[name] {
			t.Errorf("expected %s on own master with own golden, got master=%s golden=%q", name, m, golden)
		}
	}
}

// TestEmpiBackfill_PoisonAdvancesCursor: person yang bikin ingest error
// deterministik (hash sama dengan dua master yang link aktifnya menunjuk
// person di company SAMA → UpsertEmpiCandidatePair melanggar CHECK
// person_a_company_id <> person_b_company_id) tidak boleh membuat backfill
// mengulang batch yang sama tanpa batas: kursor (created_at, id) tetap maju,
// person sebelum/sesudah "racun" tetap terproses, failed > 0.
func TestEmpiBackfill_PoisonAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	company := empiSeedCompany(t, ctx, pool, "eeeeeeee-9000-0000-0000-000000000003", "empi-bf-poison")

	// Dua master ber-hash sama di empi.person_link, link aktifnya menunjuk
	// person di company yang sama: ingest person yang membawa hash itu gagal
	// deterministik (CHECK person_a_company_id <> person_b_company_id).
	// Person target link TANPA hash — core.person punya UNIQUE (company_id,
	// nik_search_hash), hash hanya dibawa person "racun".
	m1 := "eeeeeeee-9000-0000-0000-0000000000a1"
	m2 := "eeeeeeee-9000-0000-0000-0000000000a2"
	empiSeedMaster(t, ctx, pool, m1, "M Satu", nil)
	empiSeedMaster(t, ctx, pool, m2, "M Dua", nil)
	ly := empiInsertPerson(t, ctx, pool, company, "L Y", "", "1980-01-01")
	lz := empiInsertPerson(t, ctx, pool, company, "L Z", "", "1981-02-02")
	empiSeedLink(t, ctx, pool, "eeeeeeee-9000-0000-0000-0000000000b1", m1, company, ly, empiHashH1, "L Y", nil)
	empiSeedLink(t, ctx, pool, "eeeeeeee-9000-0000-0000-0000000000b2", m2, company, lz, empiHashH1, "L Z", nil)

	// Urutan created_at eksplisit: racun PALING AWAL — bila ly/lz (yang link
	// seed-nya membawa hash) terlanjur di-ingest lebih dulu, UpsertEmpiPersonLink
	// memindahkan link seed itu dan menghapus hash-nya, sehingga racun tidak
	// gagal lagi. Dengan racun pertama, batch-nya gagal + rollback (link seed
	// tetap utuh), lalu ly/lz di-ingest normal.
	ok1 := empiInsertPerson(t, ctx, pool, company, "Ok Satu", "", "1990-01-01")
	poison := empiInsertPerson(t, ctx, pool, company, "Racun", empiHashH1, "1990-06-06")
	ok2 := empiInsertPerson(t, ctx, pool, company, "Ok Dua", "", "1991-03-03")
	setCreatedAt := func(pid string, minute int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			"UPDATE core.person SET created_at = $1 WHERE id = $2",
			time.Date(2026, 1, 3, 10, minute, 0, 0, time.UTC), pid); err != nil {
			t.Fatalf("set created_at %s: %v", pid, err)
		}
	}
	setCreatedAt(poison, 0)
	setCreatedAt(ly, 1)
	setCreatedAt(lz, 2)
	setCreatedAt(ok1, 3)
	setCreatedAt(ok2, 4)

	// batchSize=1: poison gagal (CHECK), ly/lz/ok1/ok2 sukses — kursor harus
	// tetap maju saat poison gagal (sebaliknya loop tak berujung di batch itu).
	processed, failed, err := server.BackfillEmpiCompany(ctx, pool, empiUUID(t, company), 1)
	if err != nil {
		t.Fatalf("backfill with poison: %v", err)
	}
	// Hitungan dari definisi: poison gagal (1); ly, lz, ok1, ok2 sukses (4).
	if processed != 4 || failed != 1 {
		t.Errorf("expected processed=4 failed=1, got processed=%d failed=%d", processed, failed)
	}
	// Racun sendiri TIDAK punya link (batch-nya rollback).
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.person_link WHERE person_id = $1", poison); got != 0 {
		t.Errorf("expected no link for rolled-back poison, got %d", got)
	}
	for name, pid := range map[string]string{"ok1": ok1, "ok2": ok2} {
		if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.person_link WHERE person_id = $1 AND status='active'", pid); got != 1 {
			t.Errorf("expected %s ingested despite poison batches, got %d links", name, got)
		}
	}
}

// empiSetPhone sets core.person.phone before ingest (helper test probabilistik).
func empiSetPhone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, personID, phone string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE core.person SET phone = $1 WHERE id = $2", phone, personID); err != nil {
		t.Fatalf("set phone %s: %v", personID, err)
	}
}

// empiDismissAllProbabilistic dismisses every candidate_pair (UPDATE langsung,
// pola TestEmpiIngest_SameNikReopensDismissedPair).
func empiDismissAllProbabilistic(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"UPDATE empi.candidate_pair SET status='dismissed', decided_by=$1, decided_at=now() WHERE reason IN ('birth_gender_name','phone_name')",
		"eeeeeeee-b100-0000-0000-0000000000d1"); err != nil {
		t.Fatalf("dismiss pairs: %v", err)
	}
}

// TestEmpiProbabilistic_BirthGenderName (plan G2 Task 2, spec §6.2): dua company,
// dob+gender sama, nama mirip → 1 pair birth_gender_name skor 80 pending.
func TestEmpiProbabilistic_BirthGenderName(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-b200-0000-0000-000000000001", "empi-pb-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-b200-0000-0000-000000000002", "empi-pb-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", "", "1990-01-01")
	empiIngest(t, ctx, pool, companyA, a1)
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah.", "", "1990-01-01")
	empiIngest(t, ctx, pool, companyB, b1)

	// Hitungan dari definisi: cabang birth_gender_name (dob sama + gender sama +
	// similarity >= 0.5) tercapai, cabang phone tidak (phone kosong) → skor 80.
	var score int32
	var reason, status string
	if err := pool.QueryRow(ctx,
		"SELECT score, reason, status FROM empi.candidate_pair").Scan(&score, &reason, &status); err != nil {
		t.Fatalf("read candidate pair: %v", err)
	}
	if reason != "birth_gender_name" || score != 80 || status != "pending" {
		t.Errorf("expected birth_gender_name/80/pending, got %s/%d/%s", reason, score, status)
	}
}

// TestEmpiProbabilistic_PhoneName: phone sama + nama mirip, dob beda → skor 70
// phone_name (cabang birth tidak tercapai karena dob beda).
func TestEmpiProbabilistic_PhoneName(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-b300-0000-0000-000000000001", "empi-pp-a")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-b300-0000-0000-000000000002", "empi-pp-b")

	a1 := empiInsertPerson(t, ctx, pool, companyA, "Budi Santoso", "", "1980-01-01")
	empiSetPhone(t, ctx, pool, a1, "081111110001")
	empiIngest(t, ctx, pool, companyA, a1)
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Budi Santoso.", "", "1985-05-05")
	empiSetPhone(t, ctx, pool, b1, "081111110001")
	empiIngest(t, ctx, pool, companyB, b1)

	var score int32
	var reason, status string
	if err := pool.QueryRow(ctx,
		"SELECT score, reason, status FROM empi.candidate_pair").Scan(&score, &reason, &status); err != nil {
		t.Fatalf("read candidate pair: %v", err)
	}
	if reason != "phone_name" || score != 70 || status != "pending" {
		t.Errorf("expected phone_name/70/pending, got %s/%d/%s", reason, score, status)
	}
}

// TestEmpiProbabilistic_ExcludesSameCompanyDismissedSameMaster: (a) satu company
// → tanpa pair; (b) pair dismissed (kedua cabang skor) tidak dibuat ulang saat
// re-ingest (NOT EXISTS); (c) dua person sama hash (satu master) → tanpa pair.
func TestEmpiProbabilistic_ExcludesSameCompanyDismissedSameMaster(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	// (a) dua person SATU company, dob+gender sama, nama mirip → tanpa pair.
	companyA := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000001", "empi-px-a")
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", "", "1990-01-01")
	empiIngest(t, ctx, pool, companyA, a1)
	a2 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah.", "", "1990-01-01")
	empiIngest(t, ctx, pool, companyA, a2)
	if got := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.candidate_pair"); got != 0 {
		t.Errorf("(a) same-company: expected 0 pair, got %d", got)
	}

	// (b) kedua cabang skor: pasangan birth_gender_name (company B/C) dan
	// phone_name (company D/E) di-dismiss, lalu re-ingest → tetap dismissed.
	// Dob unik per section: section lain tidak ikut menghasilkan pair.
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000002", "empi-px-b")
	companyC := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000003", "empi-px-c")
	bb1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", "", "1991-02-02")
	empiIngest(t, ctx, pool, companyB, bb1)
	cb1 := empiInsertPerson(t, ctx, pool, companyC, "Siti Aminah.", "", "1991-02-02")
	empiIngest(t, ctx, pool, companyC, cb1)

	companyD := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000004", "empi-px-d")
	companyE := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000005", "empi-px-e")
	dd1 := empiInsertPerson(t, ctx, pool, companyD, "Budi Santoso", "", "1980-01-01")
	empiSetPhone(t, ctx, pool, dd1, "081111110002")
	empiIngest(t, ctx, pool, companyD, dd1)
	ee1 := empiInsertPerson(t, ctx, pool, companyE, "Budi Santoso.", "", "1985-05-05")
	empiSetPhone(t, ctx, pool, ee1, "081111110002")
	empiIngest(t, ctx, pool, companyE, ee1)

	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE reason='birth_gender_name'"); got != 1 {
		t.Fatalf("(b) precondition: expected 1 birth_gender_name pair, got %d", got)
	}
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE reason='phone_name'"); got != 1 {
		t.Fatalf("(b) precondition: expected 1 phone_name pair, got %d", got)
	}
	empiDismissAllProbabilistic(t, ctx, pool)

	empiIngest(t, ctx, pool, companyB, bb1)
	empiIngest(t, ctx, pool, companyC, cb1)
	empiIngest(t, ctx, pool, companyD, dd1)
	empiIngest(t, ctx, pool, companyE, ee1)
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE status='dismissed'"); got != 2 {
		t.Errorf("(b) after re-ingest: expected 2 pairs still dismissed, got %d", got)
	}
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair WHERE status<>'dismissed'"); got != 0 {
		t.Errorf("(b) after re-ingest: expected no non-dismissed pair, got %d", got)
	}

	// (c) dua person sama hash lintas company (dob+nama mirip) → auto-link satu
	// master → tanpa pair probabilistik (master_person_id harus berbeda).
	companyF := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000006", "empi-px-f")
	f1 := empiInsertPerson(t, ctx, pool, companyF, "Tini Aminah", empiHashH1, "1992-03-03")
	empiIngest(t, ctx, pool, companyF, f1)
	companyG := empiSeedCompany(t, ctx, pool, "eeeeeeee-b400-0000-0000-000000000007", "empi-px-g")
	g1 := empiInsertPerson(t, ctx, pool, companyG, "Tini Aminah", empiHashH1, "1992-03-03")
	empiIngest(t, ctx, pool, companyG, g1)
	var sameMaster int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM empi.person_link l1 JOIN empi.person_link l2 ON l1.master_person_id = l2.master_person_id WHERE l1.person_id = $1 AND l2.person_id = $2",
		f1, g1).Scan(&sameMaster); err != nil {
		t.Fatalf("(c) join links: %v", err)
	}
	if sameMaster != 1 {
		t.Fatalf("(c) precondition: expected F1+G1 on same master, got %d", sameMaster)
	}
	if got := empiCount(t, ctx, pool,
		"SELECT count(*) FROM empi.candidate_pair cp JOIN empi.person_link la ON la.person_id = cp.person_a_id JOIN empi.person_link lb ON lb.person_id = cp.person_b_id WHERE la.person_id IN ($1,$2) OR lb.person_id IN ($1,$2)",
		f1, g1); got != 0 {
		t.Errorf("(c) same-master: expected 0 probabilistic pair, got %d", got)
	}
}
