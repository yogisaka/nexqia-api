// Command import-growth-curve loads local WHO Child Growth Standards CSV
// dumps (data-import/growth-curve/*.csv) into ref.growth_curve without any
// network access. CSV preparation is an operator task: download the expanded
// WHO tables per indicator x gender and save them as deterministic CSV files
// with the literal header indicator,gender,x_kind,x,l,m,s.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	sourceName    = "WHO Child Growth Standards"
	sourceVersion = "who-cgs-2006"
)

// sourceURL maps each indicator to its WHO standards page (spec R:44).
var sourceURL = map[string]string{
	"bbu":  "https://www.who.int/tools/child-growth-standards/standards/bbu",
	"tbu":  "https://www.who.int/tools/child-growth-standards/standards/tbu",
	"bbtb": "https://www.who.int/tools/child-growth-standards/standards/weight-for-height",
	"imtu": "https://www.who.int/tools/child-growth-standards/standards/imtu",
	"lku":  "https://www.who.int/tools/child-growth-standards/standards/lku",
	"lila": "https://www.who.int/tools/child-growth-standards/standards/lila",
}

var (
	validIndicators = map[string]bool{"bbu": true, "tbu": true, "bbtb": true, "imtu": true, "lku": true, "lila": true}
	validGenders    = map[string]bool{"laki_laki": true, "perempuan": true}
	validXKinds     = map[string]bool{"age_days": true, "height_cm": true}
)

const csvHeader = "indicator,gender,x_kind,x,l,m,s"

// insertSQL orders the parameters to match the ref.growth_curve columns.
const insertSQL = `INSERT INTO ref.growth_curve ` +
	`(indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at) ` +
	`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ` +
	`ON CONFLICT (indicator, gender, x, source_version) DO NOTHING`

const countSQL = `SELECT count(*) FROM ref.growth_curve WHERE indicator = $1 AND gender = $2`

type curveRow struct {
	Indicator string
	Gender    string
	XKind     string
	X         string
	L         string
	M         string
	S         string
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "usage: import-growth-curve --dir data-import/growth-curve [--dsn <DATABASE_URL>] [--dry-run] [--downloaded-at RFC3339]")
	fmt.Fprintln(os.Stderr, "persiapan CSV = operator: unduh tabel expanded WHO per indikator x gender, simpan sebagai CSV deterministik dengan header indicator,gender,x_kind,x,l,m,s")
	if fs != nil {
		fs.PrintDefaults()
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-growth-curve:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("import-growth-curve", flag.ContinueOnError)
	dir := fs.String("dir", "data-import/growth-curve", "directory containing the WHO CSV dumps (one CSV per indicator x gender)")
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "Postgres DSN (default: env DATABASE_URL)")
	dryRun := fs.Bool("dry-run", false, "parse the CSVs and report only, no database access")
	downloadedAt := fs.String("downloaded-at", "", "RFC3339 timestamp recorded as downloaded_at (default: time of run)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		usage(fs)
		os.Exit(2)
	}

	var dl time.Time
	if *downloadedAt == "" {
		dl = time.Now()
	} else {
		t, err := time.Parse(time.RFC3339, *downloadedAt)
		if err != nil {
			return fmt.Errorf("--downloaded-at bukan RFC3339: %w", err)
		}
		dl = t
	}

	rows, groups, err := loadDir(*dir)
	if err != nil {
		return err
	}
	report(rows, groups, *dir)

	if *dryRun {
		fmt.Println("dry-run: tidak ada data yang ditulis")
		return nil
	}

	if *dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := insertRows(ctx, pool, rows, dl); err != nil {
		return err
	}
	return verifyGroups(ctx, pool, groups)
}

// loadDir parses every *.csv file in dir (sorted for deterministic errors) and
// returns the rows plus the set of (indicator, gender) groups the files cover.
func loadDir(dir string) ([]curveRow, map[[2]string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("tidak ada CSV di %s — unduh tabel expanded WHO (spec §4.3)", dir)
	}
	sort.Strings(names)

	var rows []curveRow
	groups := map[[2]string]bool{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		parsed, err := parseCSV(f, name)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		for _, r := range parsed {
			groups[[2]string{r.Indicator, r.Gender}] = true
		}
		rows = append(rows, parsed...)
	}
	return rows, groups, nil
}

// parseCSV validates the literal header and each data row against the spec
// §4.1(a) enums; errors name the file and the 1-based CSV record line.
func parseCSV(r io.Reader, name string) ([]curveRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var rows []curveRow
	line := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: baris %d: %w", name, line+1, err)
		}
		line++
		if line == 1 {
			if strings.Join(rec, ",") != csvHeader {
				return nil, fmt.Errorf("%s: baris 1: header harus %q, dapat %q", name, csvHeader, strings.Join(rec, ","))
			}
			continue
		}
		if len(rec) != 7 {
			return nil, fmt.Errorf("%s: baris %d: butuh 7 kolom, dapat %d", name, line, len(rec))
		}
		ind, gen, xk := rec[0], rec[1], rec[2]
		if !validIndicators[ind] {
			return nil, fmt.Errorf("%s: baris %d: indicator %q di luar enum (bbu,tbu,bbtb,imtu,lku,lila)", name, line, ind)
		}
		if !validGenders[gen] {
			return nil, fmt.Errorf("%s: baris %d: gender %q di luar enum (laki_laki,perempuan)", name, line, gen)
		}
		if !validXKinds[xk] {
			return nil, fmt.Errorf("%s: baris %d: x_kind %q di luar enum (age_days,height_cm)", name, line, xk)
		}
		for i, col := range []string{"x", "l", "m", "s"} {
			v, err := strconv.ParseFloat(rec[3+i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s: baris %d: kolom %s: bukan angka: %q", name, line, col, rec[3+i])
			}
			if col == "x" && v <= 0 {
				return nil, fmt.Errorf("%s: baris %d: x harus > 0, dapat %s", name, line, rec[3])
			}
			if col == "s" && v <= 0 {
				return nil, fmt.Errorf("%s: baris %d: s harus > 0, dapat %s", name, line, rec[6])
			}
		}
		rows = append(rows, curveRow{Indicator: ind, Gender: gen, XKind: xk, X: rec[3], L: rec[4], M: rec[5], S: rec[6]})
	}
	if line == 0 {
		return nil, fmt.Errorf("%s: kosong (tanpa header)", name)
	}
	return rows, nil
}

// report prints per-group counts before any database access.
func report(rows []curveRow, groups map[[2]string]bool, dir string) {
	keys := sortedGroups(groups)
	fmt.Printf("dir=%s baris=%d kelompok=%d\n", dir, len(rows), len(keys))
	for _, g := range keys {
		n := 0
		for _, r := range rows {
			if r.Indicator == g[0] && r.Gender == g[1] {
				n++
			}
		}
		fmt.Printf("  %s/%s: %d baris\n", g[0], g[1], n)
	}
}

// sortedGroups returns the group keys in deterministic (indicator, gender) order.
func sortedGroups(groups map[[2]string]bool) [][2]string {
	keys := make([][2]string, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
}

// insertRows writes every parsed row; conflicts on the natural key are skipped.
func insertRows(ctx context.Context, pool *pgxpool.Pool, rows []curveRow, dl time.Time) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	inserted, skipped := 0, 0
	for _, r := range rows {
		ct, err := tx.Exec(ctx, insertSQL,
			r.Indicator, r.Gender, r.XKind, r.X, r.L, r.M, r.S,
			sourceName, sourceVersion, sourceURL[r.Indicator], dl)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			skipped++
		} else {
			inserted++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("insert=%d skip(conflict)=%d\n", inserted, skipped)
	return nil
}

// verifyGroups hard-fails when any (indicator, gender) group covered by the
// CSV input is still empty in ref.growth_curve after the load.
func verifyGroups(ctx context.Context, pool *pgxpool.Pool, groups map[[2]string]bool) error {
	var empty []string
	for _, g := range sortedGroups(groups) {
		var n int64
		if err := pool.QueryRow(ctx, countSQL, g[0], g[1]).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			empty = append(empty, fmt.Sprintf("%s/%s", g[0], g[1]))
		} else {
			fmt.Printf("  %s/%s: %d baris di ref.growth_curve\n", g[0], g[1], n)
		}
	}
	if len(empty) > 0 {
		return fmt.Errorf("kelompok kosong setelah load: %s", strings.Join(empty, ", "))
	}
	return nil
}
