// Command import-masterdata loads local master-data dumps (Kemendagri wilayah,
// ISO 3166-1 citizenship) into terminology.* without any network access.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yogisaka/nexqia-api/internal/termimport"
)

type datasetSpec struct {
	parser  func(io.Reader) (termimport.ParseResult, error)
	name    string
	uri     string
	version string
	license string
	attrib  string
	source  string
	defFile string
	preLoad func(res termimport.ParseResult)
}

var datasets = map[string]datasetSpec{
	"wilayah": {
		parser:  termimport.ParseWilayahSQL,
		name:    "kemendagri-region-2025",
		uri:     "http://www.kemendagri.go.id/region/2025",
		version: "kepmendagri-300.2.2-2138-2025",
		license: "Open data Kemendagri (mirror cahyadsn/wilayah, MIT)",
		attrib:  "Kode wilayah administrasi Kepmendagri 300.2.2-2138/2025 + pemutakhiran -2430/2025",
		source:  "https://raw.githubusercontent.com/cahyadsn/wilayah/master/db/wilayah.sql",
		defFile: "data-import/kemendagri-2025-wilayah.sql",
		preLoad: reportWilayah,
	},
	"citizenship": {
		parser:  termimport.ParseISO3166JSON,
		name:    "citizenship",
		uri:     "urn:iso:std:iso:3166:-1",
		version: "iso-3166-1",
		license: "ISO 3166-1 Country Codes",
		attrib:  "Turunan tabel resmi ISO 3166 Maintenance Agency (lukes/ISO-3166-Countries-with-Regional-Codes)",
		source:  "https://raw.githubusercontent.com/lukes/ISO-3166-Countries-with-Regional-Codes/master/all/all.json",
		defFile: "data-import/iso3166-1.json",
		preLoad: reportCitizenship,
	},
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "usage: import-masterdata <dataset> [--file <path>] [--dry-run]")
	fmt.Fprintln(os.Stderr, "datasets: wilayah, citizenship")
	if fs != nil {
		fs.PrintDefaults()
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-masterdata:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage(nil)
		os.Exit(2)
	}
	dataset := os.Args[1]

	fs := flag.NewFlagSet("import-masterdata", flag.ContinueOnError)
	file := fs.String("file", "", "read the dump from this local path (default: per-dataset path under data-import/)")
	dryRun := fs.Bool("dry-run", false, "run everything, roll back the transaction")
	if err := fs.Parse(os.Args[2:]); err != nil {
		usage(fs)
		os.Exit(2)
	}

	spec, ok := datasets[dataset]
	if !ok {
		usage(fs)
		os.Exit(2)
	}
	path := spec.defFile
	if *file != "" {
		path = *file
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "file tidak ditemukan: %s\n", path)
		os.Exit(1)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	res, err := spec.parser(f)
	f.Close()
	if err != nil {
		return err
	}
	// hash the local file for CodeSystem.SourceSHA256
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if spec.preLoad != nil {
		spec.preLoad(res)
	}

	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is not set")
		os.Exit(2)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	cs := termimport.CodeSystem{
		URI:          spec.uri,
		Name:         spec.name,
		Version:      spec.version,
		License:      spec.license,
		Attribution:  spec.attrib,
		SourceURL:    spec.source,
		SourceSHA256: hex.EncodeToString(sum[:]),
	}
	summary, err := termimport.LoadConcepts(ctx, pool, cs, res, *dryRun)
	if err != nil {
		return err
	}
	fmt.Printf("new=%d updated=%d deactivated=%d edges=%d skipped=%d\n",
		summary.New, summary.Updated, summary.Deactivated, summary.Edges, len(summary.Skipped))
	for _, line := range summary.Skipped {
		fmt.Println("skipped:", line)
	}
	return nil
}

// reportWilayah prints per-level counts (by digit length without dots:
// 2=provinsi, 4=kab-kota, 6=kecamatan, 10=desa-kelurahan) and the KAB/KOTA
// split by uppercase name prefix before loading.
func reportWilayah(res termimport.ParseResult) {
	counts := map[int]int{}
	kab, kota := 0, 0
	for _, c := range res.Concepts {
		digits := strings.ReplaceAll(c.Code, ".", "")
		counts[len(digits)]++
		if len(digits) == 4 {
			if strings.HasPrefix(strings.ToUpper(c.Display), "KOTA") {
				kota++
			} else {
				kab++
			}
		}
	}
	fmt.Printf("wilayah: provinsi=%d kab-kota=%d kecamatan=%d desa-kelurahan=%d\n",
		counts[2], counts[4], counts[6], counts[10])
	fmt.Printf("wilayah: KAB=%d KOTA=%d\n", kab, kota)
}

// reportCitizenship prints the number of countries before loading.
func reportCitizenship(res termimport.ParseResult) {
	fmt.Printf("citizenship: negara=%d\n", len(res.Concepts))
}
