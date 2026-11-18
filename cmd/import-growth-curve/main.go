// Command import-growth-curve loads local WHO growth curve CSV dumps
// (data-import/growth-curve/*.csv = Child Growth Standards 2006, 0–60 bln;
// --gr-dir = Growth Reference 2007, 61 bln s.d. domain anak) into
// ref.growth_curve without any network access. CSV preparation is an
// operator task: download the expanded WHO tables per indicator x gender and
// save them as deterministic CSV files with the literal header
// indicator,gender,x_kind,x,l,m,s. File GR 2007 memakai x dalam BULAN —
// loader mengonversi x_days = round(x_bulan × 30.4375) sebelum insert
// (D-KA-14, plan 2026-11-18-anak-a1-1-be Task 4).
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// datasetGroup — konstanta satu sumber kurva (grup kedua who-gr-2007:
// lampiran masterdata M2, plan 2026-11-18-anak-a1-1-be Task 4).
type datasetGroup struct {
	dir           string
	sourceName    string
	sourceVersion string
	sourceURL     map[string]string
	validIndic    map[string]bool
	xInMonths     bool // file GR memakai x bulan → konversi ke age_days
}

var validIndicators = map[string]bool{"bbu": true, "tbu": true, "bbtb": true, "imtu": true, "lku": true, "lila": true}
var grValidIndicators = map[string]bool{"bbu": true, "tbu": true, "imtu": true}
var validGenders = map[string]bool{"laki_laki": true, "perempuan": true}
var validXKinds = map[string]bool{"age_days": true, "height_cm": true}

// grSourceURL — halaman databases WHO GR 2007 per indikator, URL dari
// lampiran masterdata M2 (jangan menebak path file di bawahnya).
var grSourceURL = map[string]string{
	"bbu":  "https://www.who.int/tools/growth-reference-data-for-5to19-years/databases",
	"tbu":  "https://www.who.int/tools/growth-reference-data-for-5to19-years/databases",
	"imtu": "https://www.who.int/tools/growth-reference-data-for-5to19-years/databases",
}

// sourceURL maps each indicator to its WHO standards page (spec R:44) —
// konstanta grup CGS 2006 (dirujuk unit test existing).
var sourceURL = map[string]string{
	"bbu":  "https://www.who.int/tools/child-growth-standards/standards/bbu",
	"tbu":  "https://www.who.int/tools/child-growth-standards/standards/tbu",
	"bbtb": "https://www.who.int/tools/child-growth-standards/standards/weight-for-height",
	"imtu": "https://www.who.int/tools/child-growth-standards/standards/imtu",
	"lku":  "https://www.who.int/tools/child-growth-standards/standards/lku",
	"lila": "https://www.who.int/tools/child-growth-standards/standards/lila",
}

// datasetGroups: grup 1 = WHO CGS 2006 (spec R:44), grup 2 = WHO GR 2007 (M2).
func datasetGroups(cgsDir, grDir string) []datasetGroup {
	return []datasetGroup{
		{
			dir: cgsDir, sourceName: "WHO Child Growth Standards", sourceVersion: "who-cgs-2006",
			sourceURL:  sourceURL,
			validIndic: validIndicators,
		},
		{
			dir: grDir, sourceName: "WHO Growth Reference 2007", sourceVersion: "who-gr-2007",
			sourceURL: grSourceURL, validIndic: grValidIndicators, xInMonths: true,
		},
	}
}

const csvHeader = "indicator,gender,x_kind,x,l,m,s"

// growthMonthDays — konversi bulan→hari D-KA-14 (sinkron server:
// age_months = floor(age_days/30.4375)).
const growthMonthDays = 30.4375

// insertSQL orders the parameters to match the ref.growth_curve columns.
const insertSQL = `INSERT INTO ref.growth_curve ` +
	`(indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at) ` +
	`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ` +
	`ON CONFLICT (indicator, gender, x, source_version) DO NOTHING`

const countSQL = `SELECT count(*) FROM ref.growth_curve WHERE indicator = $1 AND gender = $2`

type curveRow struct {
	Indicator     string
	Gender        string
	XKind         string
	X             string
	L             string
	M             string
	S             string
	SourceName    string
	SourceVersion string
	SourceURL     string
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "usage: import-growth-curve [--dir data-import/growth-curve] [--gr-dir data-import/growth-curve/gr-2007] [--dsn <DATABASE_URL>] [--dry-run] [--downloaded-at RFC3339]")
	fmt.Fprintln(os.Stderr, "persiapan CSV = operator: unduh tabel expanded WHO per indikator x gender, simpan sebagai CSV deterministik dengan header indicator,gender,x_kind,x,l,m,s (GR 2007: x dalam BULAN, dikonversi ke age_days)")
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
	dir := fs.String("dir", "data-import/growth-curve", "directory containing the WHO CGS 2006 CSV dumps (one CSV per indicator x gender)")
	grDir := fs.String("gr-dir", "data-import/growth-curve/gr-2007", "directory containing the WHO GR 2007 CSV dumps (x in months; empty string skips the group)")
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

	groups := datasetGroups(*dir, *grDir)
	rows, covered, err := loadGroups(groups)
	if err != nil {
		return err
	}
	report(rows, covered, groups)

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
	return verifyGroups(ctx, pool, covered)
}

// loadGroups parses the CSV dumps of every dataset group (dirs may overlap or
// be absent — a missing CGS dir with a filled GR dir is valid) and returns
// the rows plus the (indicator, gender) groups covered by the inputs.
func loadGroups(groups []datasetGroup) ([]curveRow, map[[2]string]bool, error) {
	var rows []curveRow
	covered := map[[2]string]bool{}
	loaded := 0
	for _, g := range groups {
		if g.dir == "" {
			continue
		}
		parsed, err := loadDir(g)
		if err != nil {
			if os.IsNotExist(err) && loaded > 0 {
				continue // grup kedua opsional
			}
			return nil, nil, err
		}
		loaded++
		for _, r := range parsed {
			covered[[2]string{r.Indicator, r.Gender}] = true
		}
		rows = append(rows, parsed...)
	}
	if loaded == 0 {
		return nil, nil, fmt.Errorf("tidak ada CSV yang ditemukan — unduh tabel expanded WHO (spec §4.3)")
	}
	return rows, covered, nil
}

// loadDir parses every *.csv file in the group's dir (sorted for
// deterministic errors) and stamps each row with the group's source constants.
func loadDir(g datasetGroup) ([]curveRow, error) {
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("tidak ada CSV di %s — unduh tabel expanded WHO (spec §4.3)", g.dir)
	}
	sort.Strings(names)

	var rows []curveRow
	for _, name := range names {
		path := filepath.Join(g.dir, name)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		parsed, err := parseCSVGroup(f, name, g)
		f.Close()
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
	}
	return rows, nil
}

// parseCSV mem-parsing CSV grup CGS 2006 (bentuk pemanggil lama — dipakai
// unit test existing); grup eksplisit lewat parseCSVGroup.
func parseCSV(r io.Reader, name string) ([]curveRow, error) {
	return parseCSVGroup(r, name, datasetGroups("", "")[0])
}

// parseCSVGroup validates the literal header and each data row against the spec
// §4.1(a) enums; errors name the file and the 1-based CSV record line. File
// grup GR 2007 memakai x dalam BULAN → x_days = round(x_bulan × 30.4375)
// (D-KA-14) sebelum baris distamp source grup.
func parseCSVGroup(r io.Reader, name string, g datasetGroup) ([]curveRow, error) {
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
		if !g.validIndic[ind] {
			return nil, fmt.Errorf("%s: baris %d: indicator %q di luar enum grup %s (bbu,tbu,bbtb,imtu,lku,lila)", name, line, ind, g.sourceVersion)
		}
		if !validGenders[gen] {
			return nil, fmt.Errorf("%s: baris %d: gender %q di luar enum (laki_laki,perempuan)", name, line, gen)
		}
		if !validXKinds[xk] {
			return nil, fmt.Errorf("%s: baris %d: x_kind %q di luar enum (age_days,height_cm)", name, line, xk)
		}
		xv, err := strconv.ParseFloat(rec[3], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: baris %d: kolom x: bukan angka: %q", name, line, rec[3])
		}
		if xv <= 0 {
			return nil, fmt.Errorf("%s: baris %d: x harus > 0, dapat %s", name, line, rec[3])
		}
		for i, col := range []string{"l", "m", "s"} {
			v, err := strconv.ParseFloat(rec[4+i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s: baris %d: kolom %s: bukan angka: %q", name, line, col, rec[4+i])
			}
			if col == "s" && v <= 0 {
				return nil, fmt.Errorf("%s: baris %d: s harus > 0, dapat %s", name, line, rec[6])
			}
		}
		x := rec[3]
		if g.xInMonths {
			// x CSV = bulan → age_days = round(bulan × 30.4375) (D-KA-14).
			x = strconv.FormatFloat(math.Round(xv*growthMonthDays), 'f', -1, 64)
		}
		rows = append(rows, curveRow{
			Indicator: ind, Gender: gen, XKind: xk, X: x, L: rec[4], M: rec[5], S: rec[6],
			SourceName: g.sourceName, SourceVersion: g.sourceVersion, SourceURL: g.sourceURL[ind],
		})
	}
	if line == 0 {
		return nil, fmt.Errorf("%s: kosong (tanpa header)", name)
	}
	return rows, nil
}

// report prints per-group counts before any database access.
func report(rows []curveRow, covered map[[2]string]bool, groups []datasetGroup) {
	keys := sortedGroups(covered)
	fmt.Printf("baris=%d kelompok=%d\n", len(rows), len(keys))
	for _, g := range groups {
		for _, k := range keys {
			if g.sourceURL[k[0]] == "" {
				continue
			}
			n := 0
			for _, r := range rows {
				if r.Indicator == k[0] && r.Gender == k[1] && r.SourceVersion == g.sourceVersion {
					n++
				}
			}
			if n > 0 {
				fmt.Printf("  %s/%s (%s): %d baris\n", k[0], k[1], g.sourceVersion, n)
			}
		}
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
			r.SourceName, r.SourceVersion, r.SourceURL, dl)
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
