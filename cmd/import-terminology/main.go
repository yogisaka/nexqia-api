// Command import-terminology downloads a pinned terminology release (or reads
// it from a local file), verifies it, and loads it into terminology.*.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yogisaka/nexqia-api/internal/termimport"
)

var datasets = map[string]bool{
	"icd11":           true,
	"icd9cm":          true,
	"icd10-icd11-map": true,
	"profiles":        true,
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: import-terminology <dataset> [--release X] [--file path] [--dry-run] [--accept-mapping-terms]")
	fmt.Fprintln(os.Stderr, "datasets: icd11, icd9cm, icd10-icd11-map, profiles")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-terminology:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	dataset := os.Args[1]

	fs := flag.NewFlagSet("import-terminology", flag.ContinueOnError)
	release := fs.String("release", "", "release key from the manifest (default: pinned default)")
	file := fs.String("file", "", "read the archive from this local path instead of downloading")
	dryRun := fs.Bool("dry-run", false, "run everything, roll back the transaction")
	_ = fs.Bool("accept-mapping-terms", false, "accepted for forward compatibility (no effect on icd11/icd9cm)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		usage()
		os.Exit(2)
	}

	switch dataset {
	case "icd11", "icd9cm":
		return importRelease(dataset, *release, *file, *dryRun)
	case "profiles":
		return applyProfiles()
	case "icd10-icd11-map":
		fmt.Fprintln(os.Stderr, "not implemented yet")
		os.Exit(2)
	default:
		usage()
		os.Exit(2)
	}
	return nil
}

func connect(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		os.Exit(2)
	}
	return pgxpool.New(ctx, url)
}

func importRelease(dataset, release, file string, dryRun bool) error {
	if release == "" {
		r, ok := termimport.DefaultRelease[dataset]
		if !ok {
			return fmt.Errorf("dataset %q has no default release", dataset)
		}
		release = r
	}
	rel, ok := termimport.Manifest[dataset][release]
	if !ok {
		return fmt.Errorf("dataset %q has no release %q", dataset, release)
	}

	var data []byte
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		data = b
	} else {
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Get(rel.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: status %d", rel.URL, resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		data = b
	}
	if err := termimport.VerifySHA256(data, rel.SHA256); err != nil {
		return err
	}
	if len(rel.Files) == 0 {
		return fmt.Errorf("release %q has no pinned member file", release)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	member, err := memberReader(zr, rel.Files[0])
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	var cs termimport.CodeSystem
	var res termimport.ParseResult
	switch dataset {
	case "icd11":
		res, err = termimport.ParseICD11(member)
		if err != nil {
			return err
		}
		if err := termimport.ValidateCounts(termimport.CountByClassKind(res), rel.Counts); err != nil {
			return err
		}
		cs = termimport.CodeSystem{
			URI:          "http://id.who.int/icd/release/11/mms",
			Name:         "ICD-11",
			Version:      release,
			Tags:         []string{"diagnosis"},
			License:      "CC BY-ND 3.0 IGO",
			Attribution:  "International Classification of Diseases, Eleventh Revision (ICD-11), World Health Organization (WHO) 2019 https://icd.who.int/browse11. Licensed under the Creative Commons Attribution-NoDerivatives 3.0 IGO licence (CC BY-ND 3.0 IGO).",
			SourceURL:    rel.URL,
			SourceSHA256: rel.SHA256,
		}
	case "icd9cm":
		res, err = termimport.ParseICD9CM(member)
		if err != nil {
			return err
		}
		cs = termimport.CodeSystem{
			URI:          "http://hl7.org/fhir/sid/icd-9-cm",
			Name:         "ICD-9-CM",
			Version:      release,
			Tags:         []string{"procedure"},
			License:      "Public domain (CMS)",
			SourceURL:    rel.URL,
			SourceSHA256: rel.SHA256,
		}
	}

	summary, err := termimport.LoadConcepts(ctx, pool, cs, res, dryRun)
	if err != nil {
		return err
	}
	printSummary(summary)
	if dryRun {
		return nil
	}
	applied, skipped, err := termimport.ApplyProfiles(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("profiles applied=%d skipped=%d\n", applied, len(skipped))
	for _, s := range skipped {
		fmt.Println("skipped:", s)
	}
	return nil
}

func memberReader(zr *zip.Reader, name string) (io.Reader, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("archive member %q not found", name)
}

func applyProfiles() error {
	ctx := context.Background()
	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, skipped, err := termimport.ApplyProfiles(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("profiles applied=%d skipped=%d\n", applied, len(skipped))
	for _, s := range skipped {
		fmt.Println("skipped:", s)
	}
	return nil
}

func printSummary(s termimport.Summary) {
	fmt.Printf("new=%d updated=%d deactivated=%d edges=%d skipped=%d\n",
		s.New, s.Updated, s.Deactivated, s.Edges, len(s.Skipped))
	for i, line := range s.Skipped {
		if i >= 20 {
			break
		}
		fmt.Println("skipped:", line)
	}
}
