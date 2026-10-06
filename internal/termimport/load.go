package termimport

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CodeSystem is the terminology.code_system row an import upserts (by URI).
type CodeSystem struct {
	URI, Name, Version, License, Attribution, SourceURL, SourceSHA256 string
	Tags                                                              []string
}

// Summary reports what one import did.
type Summary struct {
	New, Updated, Deactivated, Edges int
	Skipped                          []string
}

// LoadConcepts imports one parsed release into terminology.* inside a single
// transaction. With dryRun every step runs and the transaction is rolled back
// (the summary is still returned); otherwise it is committed.
func LoadConcepts(ctx context.Context, db *pgxpool.Pool, cs CodeSystem, res ParseResult, dryRun bool) (Summary, error) {
	var summary Summary
	summary.Skipped = res.Skipped

	tx, err := db.Begin(ctx)
	if err != nil {
		return summary, err
	}
	defer tx.Rollback(ctx)

	// 1. code_system upsert by URI; tags merged (existing tags kept)
	var systemID string
	err = tx.QueryRow(ctx, `
INSERT INTO terminology.code_system AS cs (system_uri, name, version, tags, license, attribution, source_url, source_sha256, imported_at)
VALUES ($1, $2, $3, COALESCE($4::text[], '{}'::text[]), $5, $6, $7, $8, now())
ON CONFLICT (system_uri) DO UPDATE SET
    name = EXCLUDED.name, version = EXCLUDED.version,
    tags = ARRAY(SELECT DISTINCT unnest(cs.tags || EXCLUDED.tags) ORDER BY 1),
    license = EXCLUDED.license, attribution = EXCLUDED.attribution,
    source_url = EXCLUDED.source_url, source_sha256 = EXCLUDED.source_sha256,
    imported_at = now(), is_active = true
RETURNING id;`,
		cs.URI, cs.Name, cs.Version, cs.Tags, cs.License, cs.Attribution, cs.SourceURL, cs.SourceSHA256,
	).Scan(&systemID)
	if err != nil {
		return summary, err
	}

	// 2. staging
	if _, err := tx.Exec(ctx, `
CREATE TEMP TABLE tmp_concept (code text PRIMARY KEY, display text NOT NULL, is_selectable boolean NOT NULL, properties jsonb NOT NULL) ON COMMIT DROP;`); err != nil {
		return summary, err
	}
	rows := make([][]any, 0, len(res.Concepts))
	for _, c := range res.Concepts {
		props := c.Properties
		if props == nil {
			props = map[string]any{}
		}
		raw, err := json.Marshal(props)
		if err != nil {
			return summary, err
		}
		rows = append(rows, []any{c.Code, c.Display, c.Selectable, json.RawMessage(raw)})
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"tmp_concept"},
			[]string{"code", "display", "is_selectable", "properties"}, pgx.CopyFromRows(rows)); err != nil {
			return summary, err
		}
	}

	// 3. merge; (xmax = 0) = inserted
	insertedRows, err := tx.Query(ctx, `
INSERT INTO terminology.concept (code_system_id, code, display, is_selectable, properties, is_active)
SELECT $1, t.code, t.display, t.is_selectable, t.properties, true FROM tmp_concept t
ON CONFLICT (code_system_id, code) DO UPDATE SET
    display = EXCLUDED.display, is_selectable = EXCLUDED.is_selectable,
    properties = EXCLUDED.properties, is_active = true
RETURNING (xmax = 0) AS inserted;`, systemID)
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

	// 4. codes missing from the release are deactivated, never deleted
	tag, err := tx.Exec(ctx, `
UPDATE terminology.concept c SET is_active = false
WHERE c.code_system_id = $1 AND c.is_active
  AND NOT EXISTS (SELECT 1 FROM tmp_concept t WHERE t.code = c.code);`, systemID)
	if err != nil {
		return summary, err
	}
	summary.Deactivated = int(tag.RowsAffected())

	// 5. hierarchy rebuilt for this code system
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE tmp_edge (parent_code text NOT NULL, child_code text NOT NULL) ON COMMIT DROP;`); err != nil {
		return summary, err
	}
	if len(res.Edges) > 0 {
		edgeRows := make([][]any, 0, len(res.Edges))
		for _, e := range res.Edges {
			edgeRows = append(edgeRows, []any{e.Parent, e.Child})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"tmp_edge"}, []string{"parent_code", "child_code"}, pgx.CopyFromRows(edgeRows)); err != nil {
			return summary, err
		}
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM terminology.concept_hierarchy h
USING terminology.concept c
WHERE h.child_concept_id = c.id AND c.code_system_id = $1;`, systemID); err != nil {
		return summary, err
	}
	tag, err = tx.Exec(ctx, `
INSERT INTO terminology.concept_hierarchy (parent_concept_id, child_concept_id, relationship_type)
SELECT p.id, ch.id, 'is-a' FROM tmp_edge e
JOIN terminology.concept p ON p.code_system_id = $1 AND p.code = e.parent_code
JOIN terminology.concept ch ON ch.code_system_id = $1 AND ch.code = e.child_code
ON CONFLICT DO NOTHING;`, systemID)
	if err != nil {
		return summary, err
	}
	summary.Edges = int(tag.RowsAffected())

	if dryRun {
		return summary, tx.Rollback(ctx)
	}
	return summary, tx.Commit(ctx)
}

// ApplyProfiles stores every embedded profile on its code_system row.
// Profiles whose code system has not been imported are reported as skipped.
func ApplyProfiles(ctx context.Context, db *pgxpool.Pool) (applied int, skipped []string, err error) {
	profiles, err := LoadProfiles()
	if err != nil {
		return 0, nil, err
	}
	for uri, raw := range profiles {
		tag, err := db.Exec(ctx, `UPDATE terminology.code_system SET profile = $2::jsonb WHERE system_uri = $1`, uri, raw)
		if err != nil {
			return applied, skipped, err
		}
		if tag.RowsAffected() == 0 {
			skipped = append(skipped, uri+": code system not imported")
			continue
		}
		applied++
	}
	return applied, skipped, nil
}
