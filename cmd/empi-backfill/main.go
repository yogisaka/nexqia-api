// cmd/empi-backfill
// One-shot EMPI backfill (spec 2026-10-20-empi-cross-company §9, plan G1 Task 4):
// ingest ulang seluruh person aktif per company lewat server.BackfillEmpiCompany.
// Idempoten (re-run aman), zero silent loss (failed > 0 = exit 1), TIDAK pernah
// mencetak kredensial/NIK/hash/nama — output hanya UUID + hitungan.
//
// Jalankan: go run ./cmd/empi-backfill [-company <uuid>] [-batch 500]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

func main() {
	_ = godotenv.Load() // dev: baca .env kalau ada; produksi pakai env asli dari compose/systemd

	cfg := config.Load()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.AppRuntimeDSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to connect to database")
		os.Exit(1)
	}
	defer pool.Close()

	// -company diparse jadi UUID dan dibandingkan binary (bukan string
	// case-sensitive); flag kosong = tanpa filter.
	var companyFilter pgtype.UUID
	flag.Func("company", "optional company UUID (only that company is backfilled)", func(s string) error {
		if err := companyFilter.Scan(s); err != nil {
			return fmt.Errorf("invalid -company UUID %q: %w", s, err)
		}
		return nil
	})
	batchSize := flag.Int("batch", 500, "persons per transaction batch")
	flag.Parse()

	q := sqlcgen.New(pool)
	var totalProcessed, totalFailed int
	offset := int32(0)
	for {
		companies, err := q.ListCompaniesForEmpiBackfill(ctx, sqlcgen.ListCompaniesForEmpiBackfillParams{
			PLimit: 500, POffset: offset,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "list companies:", err)
			os.Exit(1)
		}
		if len(companies) == 0 {
			break
		}
		for _, companyID := range companies {
			if companyFilter.Valid && companyID != companyFilter {
				continue
			}
			processed, failed, err := server.BackfillEmpiCompany(ctx, pool, companyID, *batchSize)
			if err != nil {
				fmt.Fprintln(os.Stderr, "backfill error company", companyID.String(), ":", err)
				os.Exit(1)
			}
			fmt.Printf("company=%s processed=%d failed=%d\n", companyID.String(), processed, failed)
			totalProcessed += processed
			totalFailed += failed
		}
		offset += int32(len(companies))
	}

	fmt.Printf("total processed=%d failed=%d\n", totalProcessed, totalFailed)
	if totalFailed > 0 {
		os.Exit(1)
	}
}
