// internal/server/empi_ingest.go
// EMPI ingest (spec 2026-10-20-empi-cross-company §6.1, plan G1 Task 3):
// sinkron dalam RunRequestTx pada create/update/delete person — error di sini
// membatalkan seluruh write person (fail-hard, pola jebakan profil).
// Dipakai handler person (Task 3) dan command backfill (Task 4).
package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// EmpiIngestPerson men-snapshot demografi person ke empi.person_link, menentukan
// master target (auto-link by nik_search_hash, pindah bila master yatim, kandidat
// same_nik selain itu), lalu recompute golden. HARUS dipanggil dalam tx yang sama
// dengan write person (tx-scoped advisory lock per hash mencegah race dua master).
func EmpiIngestPerson(ctx context.Context, tx pgx.Tx, companyID, personID pgtype.UUID) error {
	q := sqlcgen.New(tx)

	p, err := q.GetPersonForEmpiIngest(ctx, personID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("empi ingest: person not found: %w", err)
		}
		return fmt.Errorf("empi ingest: get person: %w", err)
	}

	// Person soft-deleted: detach link + recompute master lama; master yatim
	// dibiarkan (purge = G4).
	if p.DeletedAt.Valid {
		link, err := q.GetEmpiLinkByPerson(ctx, sqlcgen.GetEmpiLinkByPersonParams{
			CompanyID: companyID, PersonID: personID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("empi ingest: get link: %w", err)
		}
		if err == nil {
			if err := q.DetachEmpiLink(ctx, sqlcgen.DetachEmpiLinkParams{
				CompanyID: companyID, PersonID: personID,
			}); err != nil {
				return fmt.Errorf("empi ingest: detach link: %w", err)
			}
			if err := q.RecomputeEmpiMasterGolden(ctx, link.MasterPersonID); err != nil {
				return fmt.Errorf("empi ingest: recompute golden: %w", err)
			}
		}
		return nil
	}

	hasHash := p.NikSearchHash.Valid && p.NikSearchHash.String != ""
	if hasHash {
		// Advisory lock SEBELUM lookup master: serialisasi race NIK sama lintas company.
		if err := q.LockEmpiNik(ctx, p.NikSearchHash.String); err != nil {
			return fmt.Errorf("empi ingest: lock nik: %w", err)
		}
	}

	cur, err := q.GetEmpiLinkByPerson(ctx, sqlcgen.GetEmpiLinkByPersonParams{
		CompanyID: companyID, PersonID: personID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("empi ingest: get link: %w", err)
	}
	var curMaster *pgtype.UUID
	if err == nil {
		id := cur.MasterPersonID
		curMaster = &id
	}

	var targets []pgtype.UUID
	if hasHash {
		all, err := q.FindEmpiMastersByNikHash(ctx, p.NikSearchHash)
		if err != nil {
			return fmt.Errorf("empi ingest: find masters: %w", err)
		}
		for _, m := range all {
			if curMaster == nil || m != *curMaster {
				targets = append(targets, m)
			}
		}
	}

	var target pgtype.UUID
	var movedFrom *pgtype.UUID
	switch {
	case !hasHash:
		if curMaster != nil {
			target = *curMaster
		} else {
			target, err = q.InsertEmpiMasterPerson(ctx, sqlcgen.InsertEmpiMasterPersonParams{
				FullName: p.FullName, BirthDate: p.BirthDate, Gender: p.Gender,
				FullNameSourcePersonID: p.ID,
			})
			if err != nil {
				return fmt.Errorf("empi ingest: insert master: %w", err)
			}
		}
	case len(targets) == 0:
		if curMaster != nil {
			target = *curMaster
		} else {
			target, err = q.InsertEmpiMasterPerson(ctx, sqlcgen.InsertEmpiMasterPersonParams{
				FullName: p.FullName, BirthDate: p.BirthDate, Gender: p.Gender,
				FullNameSourcePersonID: p.ID,
			})
			if err != nil {
				return fmt.Errorf("empi ingest: insert master: %w", err)
			}
		}
	default: // len(targets) >= 1; targets[0] = id terkecil (query ORDER BY, deterministik)
		if curMaster == nil {
			target = targets[0] // auto-link
			// Master lain ber-hash sama (targets[1..]) → kandidat same_nik juga.
			if err := empiSameNikCandidates(ctx, q, personID, p, targets[1:]); err != nil {
				return err
			}
		} else if *curMaster == targets[0] {
			target = targets[0] // tidak berubah
		} else {
			others, err := q.CountActiveEmpiLinksByMaster(ctx, sqlcgen.CountActiveEmpiLinksByMasterParams{
				MasterPersonID: *curMaster, PersonID: personID,
			})
			if err != nil {
				return fmt.Errorf("empi ingest: count links: %w", err)
			}
			if others == 0 {
				// Master lama jadi yatim → aman dipindah; recompute master lama
				// SETELAH upsert link di bawah.
				id := *curMaster
				movedFrom = &id
				target = targets[0]
				// targets[1..] tetap jadi kandidat same_nik (kontrak plan,
				// orthogonally terhadap cabang curMaster; tidak pernah auto-merge).
				if err := empiSameNikCandidates(ctx, q, personID, p, targets[1:]); err != nil {
					return err
				}
			} else {
				// Tetap di master ini; NIK sama di master lain → kandidat same_nik.
				target = *curMaster
				if err := empiSameNikCandidates(ctx, q, personID, p, targets); err != nil {
					return err
				}
			}
		}
	}

	if _, err := q.UpsertEmpiPersonLink(ctx, sqlcgen.UpsertEmpiPersonLinkParams{
		MasterPersonID: target, CompanyID: companyID, PersonID: personID,
		NikSearchHash: p.NikSearchHash, FullName: p.FullName, BirthDate: p.BirthDate,
		Gender: p.Gender, Phone: nullIfEmpty(p.Phone),
	}); err != nil {
		return fmt.Errorf("empi ingest: upsert link: %w", err)
	}

	if movedFrom != nil {
		if err := q.RecomputeEmpiMasterGolden(ctx, *movedFrom); err != nil {
			return fmt.Errorf("empi ingest: recompute golden: %w", err)
		}
	}
	if err := q.RecomputeEmpiMasterGolden(ctx, target); err != nil {
		return fmt.Errorf("empi ingest: recompute golden: %w", err)
	}
	return nil
}

// empiSameNikCandidates membuat kandidat same_nik (skor 100) antara person yang
// di-ingest dan link aktif pertama tiap master di daftar (pola kontrak: link
// ErrNoRows → skip; pasangan sudah 'linked' → NO-OP, kontrak eksplisit pemanggil G2).
func empiSameNikCandidates(ctx context.Context, q *sqlcgen.Queries, personID pgtype.UUID, p sqlcgen.GetPersonForEmpiIngestRow, targets []pgtype.UUID) error {
	for _, t := range targets {
		other, err := q.GetActiveEmpiLinkByMaster(ctx, t)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("empi ingest: get link by master: %w", err)
		}
		if _, err := q.UpsertEmpiCandidatePair(ctx, sqlcgen.UpsertEmpiCandidatePairParams{
			Column1: personID, Column2: other.PersonID,
			Column3: p.CompanyID, Column4: other.CompanyID,
			Score: 100, Reason: "same_nik",
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("empi ingest: upsert candidate: %w", err)
		}
	}
	return nil
}

// nullIfEmpty menggantikan NULLIF(phone, ”) di sisi Go (kolom link.phone NULL
// untuk phone kosong, sehingga index partial & blocking probabilistik konsisten).
func nullIfEmpty(t pgtype.Text) pgtype.Text {
	if t.Valid && t.String == "" {
		return pgtype.Text{}
	}
	return t
}
