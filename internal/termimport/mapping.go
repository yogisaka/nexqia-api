package termimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ICD-10 system URI — the only code system whose codes are matched with the
// dot tolerated (spec §12 R4: "A00.9" == "A009").
const icd10URI = "http://hl7.org/fhir/sid/icd-10"

// MapRow is one source→target pair from a WHO crosswalk file.
type MapRow struct {
	SourceCode, TargetCode string
	Preferred              bool
}

// StemCode returns the part before the first "&" or "/": ICD-11 cluster and
// block-post coordinates map onto their stem concept.
func StemCode(code string) string {
	if i := strings.IndexAny(code, "&/"); i >= 0 {
		return code[:i]
	}
	return code
}

// readTSV reads a UTF-8 tab-separated file, stripping the BOM and \r, and
// returns rows as maps keyed by header name. Headers are matched by NAME, not
// position (the WHO files mix column orders between the multi- and one-target
// crosswalks).
func readTSV(r io.Reader) ([]map[string]string, []string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	// The WHO TSV files do not quote fields, so encoding/csv would fail on a
	// bare quote inside a title; split manually instead.
	text := strings.TrimPrefix(string(data), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, nil, errors.New("empty TSV")
	}
	header := strings.Split(lines[0], "\t")
	rows := make([]map[string]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		rec := strings.Split(line, "\t")
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(rec) {
				row[name] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, header, nil
}

// requireHeaders fails when a required named column is missing: a WHO file
// with the wrong header would otherwise parse to zero rows and, in a non
// dry-run LoadMap, deactivate the whole map set without an error.
func requireHeaders(header []string, cols ...string) error {
	present := make(map[string]bool, len(header))
	for _, name := range header {
		present[name] = true
	}
	for _, col := range cols {
		if !present[col] {
			return fmt.Errorf("TSV header is missing required column %q", col)
		}
	}
	return nil
}

// ParseMap10To11 parses the WHO ICD-10→ICD-11 crosswalk. multi is the
// many-target file (one ICD-10 code to several ICD-11 codes), one the
// single-target file used to mark preferred pairs.
func ParseMap10To11(multi, one io.Reader) ([]MapRow, error) {
	multiRows, multiHeader, err := readTSV(multi)
	if err != nil {
		return nil, fmt.Errorf("10To11 multi-category: %w", err)
	}
	if err := requireHeaders(multiHeader, "10ClassKind", "icd10Code", "icd11Code"); err != nil {
		return nil, fmt.Errorf("10To11 multi-category: %w", err)
	}
	oneRows, oneHeader, err := readTSV(one)
	if err != nil {
		return nil, fmt.Errorf("10To11 one-category: %w", err)
	}
	if err := requireHeaders(oneHeader, "icd10Code", "icd11Code"); err != nil {
		return nil, fmt.Errorf("10To11 one-category: %w", err)
	}

	pref := map[string]bool{}
	for _, r := range oneRows {
		if r["icd10Code"] != "" && r["icd11Code"] != "" {
			pref[r["icd10Code"]+"|"+r["icd11Code"]] = true
		}
	}

	seen := map[string]bool{}
	var rows []MapRow
	for _, r := range multiRows {
		kind, src, tgt := r["10ClassKind"], r["icd10Code"], r["icd11Code"]
		if kind != "category" || src == "" || tgt == "" {
			continue
		}
		key := src + "|" + tgt
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, MapRow{SourceCode: src, TargetCode: tgt, Preferred: pref[key]})
	}
	return rows, nil
}

// ParseMap11To10 parses the WHO ICD-11→ICD-10 crosswalk; every pair it emits
// is preferred (the release ships a single one-target file per direction).
func ParseMap11To10(r io.Reader) ([]MapRow, error) {
	tsv, header, err := readTSV(r)
	if err != nil {
		return nil, fmt.Errorf("11To10: %w", err)
	}
	if err := requireHeaders(header, "icd11Code", "icd10Code"); err != nil {
		return nil, fmt.Errorf("11To10: %w", err)
	}
	seen := map[string]bool{}
	var rows []MapRow
	for _, row := range tsv {
		src, tgt := row["icd11Code"], row["icd10Code"]
		if src == "" || tgt == "" {
			continue
		}
		key := src + "|" + tgt
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, MapRow{SourceCode: src, TargetCode: tgt, Preferred: true})
	}
	return rows, nil
}

// LoadMap imports parsed crosswalk rows into terminology.concept_map inside a
// single transaction (dry-run rolls back). Codes are resolved in Go against
// the active concepts of both systems; unknown codes are skipped, not fatal.
func LoadMap(ctx context.Context, db *pgxpool.Pool, mapSet, sourceURI, targetURI string, rows []MapRow, dryRun bool) (Summary, error) {
	var summary Summary

	tx, err := db.Begin(ctx)
	if err != nil {
		return summary, err
	}
	defer tx.Rollback(ctx)

	// concept lookups; the ICD-10 system also matches dotless codes (§12 R4)
	sourceIDs, err := loadActiveConcepts(ctx, tx, sourceURI)
	if err != nil {
		return summary, err
	}
	targetIDs, err := loadActiveConcepts(ctx, tx, targetURI)
	if err != nil {
		return summary, err
	}

	// resolve + dedupe (source_concept_id, target_code), Preferred = OR
	type resolved struct {
		sourceID, targetID, targetCode string
		preferred                      bool
	}
	byKey := map[string]*resolved{}
	for _, row := range rows {
		srcCode := row.SourceCode
		if sourceURI == icd10URI {
			// same dotless key the map was built with (§12 R4)
			srcCode = strings.ReplaceAll(srcCode, ".", "")
		}
		srcID, ok := sourceIDs[srcCode]
		if !ok {
			summary.Skipped = append(summary.Skipped,
				fmt.Sprintf("%s %s→%s: source not found", mapSet, row.SourceCode, row.TargetCode))
			continue
		}
		tgtKey := StemCode(row.TargetCode)
		if targetURI == icd10URI {
			// dotless tolerance applies to the target side too (§12 R4)
			tgtKey = strings.ReplaceAll(tgtKey, ".", "")
		}
		tgtID, ok := targetIDs[tgtKey]
		if !ok {
			summary.Skipped = append(summary.Skipped,
				fmt.Sprintf("%s %s→%s: target not found", mapSet, row.SourceCode, row.TargetCode))
			continue
		}
		key := srcID + "|" + row.TargetCode
		if r, ok := byKey[key]; ok {
			r.preferred = r.preferred || row.Preferred
			continue
		}
		byKey[key] = &resolved{sourceID: srcID, targetID: tgtID, targetCode: row.TargetCode, preferred: row.Preferred}
	}

	if _, err := tx.Exec(ctx, `
CREATE TEMP TABLE tmp_map (source_concept_id uuid NOT NULL, target_concept_id uuid NOT NULL, target_code text NOT NULL, is_preferred boolean NOT NULL) ON COMMIT DROP;`); err != nil {
		return summary, err
	}
	if len(byKey) > 0 {
		tmp := make([][]any, 0, len(byKey))
		for _, r := range byKey {
			tmp = append(tmp, []any{r.sourceID, r.targetID, r.targetCode, r.preferred})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"tmp_map"},
			[]string{"source_concept_id", "target_concept_id", "target_code", "is_preferred"}, pgx.CopyFromRows(tmp)); err != nil {
			return summary, err
		}
	}

	insertedRows, err := tx.Query(ctx, `
INSERT INTO terminology.concept_map (map_set, source_concept_id, target_concept_id, target_code, relationship, is_preferred, is_active)
SELECT $1, m.source_concept_id, m.target_concept_id, m.target_code, 'related-to', m.is_preferred, true FROM tmp_map m
ON CONFLICT (map_set, source_concept_id, target_code) DO UPDATE SET
    target_concept_id = EXCLUDED.target_concept_id, is_preferred = EXCLUDED.is_preferred, is_active = true
RETURNING (xmax = 0) AS inserted;`, mapSet)
	if err != nil {
		return summary, err
	}
	for insertedRows.Next() {
		var inserted bool
		if err := insertedRows.Scan(&inserted); err != nil {
			insertedRows.Close()
			return summary, err
		}
		if inserted {
			summary.New++
		} else {
			summary.Updated++
		}
	}
	if err := insertedRows.Err(); err != nil {
		return summary, err
	}
	insertedRows.Close()

	tag, err := tx.Exec(ctx, `
UPDATE terminology.concept_map cm SET is_active = false
WHERE cm.map_set = $1 AND cm.is_active
  AND NOT EXISTS (SELECT 1 FROM tmp_map m WHERE m.source_concept_id = cm.source_concept_id AND m.target_code = cm.target_code);`, mapSet)
	if err != nil {
		return summary, err
	}
	summary.Deactivated = int(tag.RowsAffected())

	if dryRun {
		return summary, tx.Rollback(ctx)
	}
	return summary, tx.Commit(ctx)
}

// loadActiveConcepts maps every active code of one system to its concept id.
// For ICD-10 an additional dotless key per code tolerates codes imported or
// queried without the dot.
func loadActiveConcepts(ctx context.Context, tx pgx.Tx, systemURI string) (map[string]string, error) {
	var exists string
	if err := tx.QueryRow(ctx, `SELECT 'ok' FROM terminology.code_system WHERE system_uri = $1`, systemURI).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("code system %s not in DB: import %s first", systemURI, systemURI)
		}
		return nil, err
	}
	rows, err := tx.Query(ctx, `
SELECT c.code, c.id FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE cs.system_uri = $1 AND c.is_active;`, systemURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]string{}
	for rows.Next() {
		var code, id string
		if err := rows.Scan(&code, &id); err != nil {
			return nil, err
		}
		ids[code] = id
		if systemURI == icd10URI {
			dotless := strings.ReplaceAll(code, ".", "")
			if _, exists := ids[dotless]; !exists {
				ids[dotless] = id
			}
		}
	}
	return ids, rows.Err()
}
