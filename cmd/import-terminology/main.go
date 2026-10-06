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
	"strings"
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

func usage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "usage: import-terminology <dataset> [flags]")
	fmt.Fprintln(os.Stderr, "datasets: icd11, icd9cm, icd10-icd11-map, profiles")
	if fs != nil {
		fs.PrintDefaults()
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-terminology:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage(nil)
		os.Exit(2)
	}
	dataset := os.Args[1]

	fs := flag.NewFlagSet("import-terminology", flag.ContinueOnError)
	release := fs.String("release", "", "release key from the manifest (default: pinned default)")
	file := fs.String("file", "", "read the archive from this local path instead of downloading")
	dryRun := fs.Bool("dry-run", false, "run everything, roll back the transaction")
	accept := fs.Bool("accept-mapping-terms", false, "accepted for forward compatibility (no effect on icd11/icd9cm)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		usage(fs)
		os.Exit(2)
	}

	switch dataset {
	case "icd11", "icd9cm":
		return importRelease(dataset, *release, *file, *dryRun)
	case "profiles":
		return applyProfiles()
	case "icd10-icd11-map":
		return importMap(*release, *file, *dryRun, *accept)
	default:
		usage(fs)
		os.Exit(2)
	}
	return nil
}

func connect(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is not set")
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
	defer member.Close()

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

func memberReader(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("archive member %q not found", name)
}

// importMap loads the WHO ICD-10↔ICD-11 crosswalk into two map sets. Mapping
// terms are outside the ICD-11 licence, so they are gated behind an explicit
// dev-only flag (spec 2026-10-01-terminology-import §3.4).
func importMap(release, file string, dryRun, acceptMappingTerms bool) error {
	const licenseWarning = "mapping/crosswalk files are not covered by the ICD-11 licence (1.2.4); rerun with --accept-mapping-terms only on dev until WHO confirms in writing (spec 2026-10-01-terminology-import §3.4)"
	if !acceptMappingTerms {
		fmt.Fprintln(os.Stderr, licenseWarning)
		os.Exit(2)
	}
	if release == "" {
		release = termimport.DefaultRelease["icd10-icd11-map"]
	}
	rel, ok := termimport.Manifest["icd10-icd11-map"][release]
	if !ok {
		return fmt.Errorf("dataset %q has no release %q", "icd10-icd11-map", release)
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
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	multi, err := memberReader(zr, rel.Files[0])
	if err != nil {
		return err
	}
	defer multi.Close()
	one, err := memberReader(zr, rel.Files[1])
	if err != nil {
		return err
	}
	defer one.Close()
	reverse, err := memberReader(zr, rel.Files[2])
	if err != nil {
		return err
	}
	defer reverse.Close()

	to11, err := termimport.ParseMap10To11(multi, one)
	if err != nil {
		return err
	}
	to10, err := termimport.ParseMap11To10(reverse)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, m := range []struct {
		mapSet, sourceURI, targetURI string
		rows                         []termimport.MapRow
	}{
		{"who-icd10-to-icd11-" + release, "http://hl7.org/fhir/sid/icd-10", "http://id.who.int/icd/release/11/mms", to11},
		{"who-icd11-to-icd10-" + release, "http://id.who.int/icd/release/11/mms", "http://hl7.org/fhir/sid/icd-10", to10},
	} {
		summary, err := termimport.LoadMap(ctx, pool, m.mapSet, m.sourceURI, m.targetURI, m.rows, dryRun)
		if err != nil {
			return err
		}
		fmt.Printf("%s: new=%d updated=%d deactivated=%d edges=%d skipped=%d\n",
			m.mapSet, summary.New, summary.Updated, summary.Deactivated, summary.Edges, len(summary.Skipped))
		for i, line := range summary.Skipped {
			if i >= 20 {
				break
			}
			fmt.Println("skipped:", line)
		}
		counts := map[string]int{}
		for _, line := range summary.Skipped {
			switch {
			case strings.HasSuffix(line, "source not found"):
				counts["source not found"]++
			case strings.HasSuffix(line, "target not found"):
				counts["target not found"]++
			default:
				counts["other"]++
			}
		}
		for reason, n := range counts {
			fmt.Printf("skipped by reason %s: %d\n", reason, n)
		}
	}
	return nil
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
