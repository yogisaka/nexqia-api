// internal/server/clinical_diagnosis.go
// Dual-coded visit diagnosis persisted at clinical form signing (spec
// 2026-10-21-cppt-rajal-fase1-design §4.4, §5.2, §7; plan A Task 4).
// Runs inside the ambient RunRequestTx of the sign handler — client errors
// (invalid shape, unresolvable concept) surface as errInvalidDiagnosis → 400,
// which auto-rolls the whole sign transaction back.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// errInvalidDiagnosis marks a client-side diagnosis payload problem: the sign
// handler answers 400 {"error":"invalid diagnosis entry"} and RunRequestTx
// rolls the sign write back (spec §6.1 — entry stays draft).
var errInvalidDiagnosis = errors.New("invalid diagnosis entry")

// errSuspectNotBoolean marks a suspect flag that is present but not a JSON
// boolean/null (spec 2a §6.4/A1, remedial 6536088): the sign handler answers
// 400 {"error":"suspect harus boolean"} and RunRequestTx rolls the sign write
// back (entry stays draft). Checked at sign time only — create accepts any
// diagnoses shape.
var errSuspectNotBoolean = errors.New("suspect harus boolean")

// conceptRef is the LITERAL spec §5.2 shape {system, code, display}.
type conceptRef struct {
	System  string
	Code    string
	Display string
}

// icd10Ref is the optional spec §5.2 shape
// {code, display?, source: "auto"|"manual", map_set?}.
type icd10Ref struct {
	Code    string
	Display string
	Source  string
	MapSet  string
}

type diagnosisInput struct {
	Role    string
	Concept conceptRef
	Icd10   *icd10Ref
}

// assessmentDiagnoses parses sections.assessment.diagnoses from the decoded
// form_data (spec §5.2 LITERAL). Missing section/keys mean no diagnoses; a
// present but malformed entry returns errInvalidDiagnosis.
func assessmentDiagnoses(formData map[string]any) ([]diagnosisInput, error) {
	sections, _ := formData["sections"].(map[string]any)
	if sections == nil {
		return nil, nil
	}
	assessment, _ := sections["assessment"].(map[string]any)
	if assessment == nil {
		return nil, nil
	}
	raw, present := assessment["diagnoses"]
	if !present || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errInvalidDiagnosis
	}
	out := make([]diagnosisInput, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errInvalidDiagnosis
		}
		role, _ := m["role"].(string)
		if role != "primary" && role != "secondary" {
			return nil, errInvalidDiagnosis
		}
		cm, ok := m["concept"].(map[string]any)
		if !ok {
			return nil, errInvalidDiagnosis
		}
		system, _ := cm["system"].(string)
		code, _ := cm["code"].(string)
		display, _ := cm["display"].(string)
		if system == "" || code == "" || display == "" {
			return nil, errInvalidDiagnosis
		}
		in := diagnosisInput{Role: role, Concept: conceptRef{System: system, Code: code, Display: display}}
		if raw10 := m["icd10"]; raw10 != nil {
			m10, ok := raw10.(map[string]any)
			if !ok {
				return nil, errInvalidDiagnosis
			}
			code10, _ := m10["code"].(string)
			if code10 == "" {
				return nil, errInvalidDiagnosis
			}
			source, _ := m10["source"].(string)
			if source != "auto" && source != "manual" {
				return nil, errInvalidDiagnosis
			}
			display10, _ := m10["display"].(string)
			mapSet, _ := m10["map_set"].(string)
			in.Icd10 = &icd10Ref{Code: code10, Display: display10, Source: source, MapSet: mapSet}
		}
		// Suspect flag (spec 2a §6.4/A1, remedial 6536088): checked AFTER the
		// fase-1 shape parse (wrong shape still → errInvalidDiagnosis) and
		// before any diagnosis row is written. Key present must be a JSON
		// boolean or null; anything else → errSuspectNotBoolean.
		if rawSus, ok := m["suspect"]; ok && rawSus != nil {
			if _, isBool := rawSus.(bool); !isBool {
				return nil, errSuspectNotBoolean
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// resolveDiagnosisConcept looks up the submitted concept via the same exact
// lookup the terminology map endpoint uses (GetActiveConceptBySystemName —
// active AND selectable concept in an active code_system, so ICD-11 heading
// concepts are rejected), plus the merchant's diagnosis standard from
// core.diagnosis_config (merchant override → company default). Unknown
// concept → pgx.ErrNoRows.
func resolveDiagnosisConcept(ctx context.Context, q *sqlcgen.Queries, companyID, merchantID pgtype.UUID, ref conceptRef) (sqlcgen.GetActiveConceptBySystemNameRow, string, error) {
	concept, err := q.GetActiveConceptBySystemName(ctx, sqlcgen.GetActiveConceptBySystemNameParams{System: ref.System, Code: ref.Code})
	if err != nil {
		return concept, "", err
	}
	std, err := resolveDiagnosisCodeSystem(ctx, q, companyID, merchantID)
	return concept, std, err
}

// persistSignDiagnoses writes one clinical.diagnosis row per parsed diagnosis
// input (spec §4.4/§7). Immutable: no UPDATE/DELETE endpoint exists (STD-03).
// The primary snapshot is the resolved concept (primary_system/code/display);
// when the merchant standard is ICD-10 the primary IS the ICD-10 code, so the
// dual ICD-10 columns stay empty (spec §7.3); otherwise they carry the
// submitted icd10 mapping, which may be absent until the ICD-10 license
// clearing (spec §7.4). icd10_chosen_by = signer; chosen_at only for manual.
func persistSignDiagnoses(c *gin.Context, q *sqlcgen.Queries, entry sqlcgen.ClinicalFormFormInstance, signerID pgtype.UUID, formData map[string]any) error {
	inputs, err := assessmentDiagnoses(formData)
	if err != nil || len(inputs) == 0 {
		return err
	}
	ctx := c.Request.Context()
	for _, in := range inputs {
		concept, std, err := resolveDiagnosisConcept(ctx, q, entry.CompanyID, entry.MerchantID, in.Concept)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: concept %s/%s not found", errInvalidDiagnosis, in.Concept.System, in.Concept.Code)
		}
		if err != nil {
			return err
		}
		// Merchant standard ICD-10 + non-ICD-10 concept without a submitted
		// mapping → client error (review A2): the primary snapshot must be a
		// real ICD-10 code, not a stand-in (spec §7.3).
		if std == defaultDiagnosisCodeSystem && in.Concept.System != defaultDiagnosisCodeSystem && in.Icd10 == nil {
			return fmt.Errorf("%w: icd10 mapping required for %s/%s", errInvalidDiagnosis, in.Concept.System, in.Concept.Code)
		}
		p := sqlcgen.CreateVisitDiagnosisParams{
			CompanyID:      entry.CompanyID,
			MerchantID:     entry.MerchantID,
			AdmissionID:    entry.AdmissionID,
			FormInstanceID: entry.ID,
			DxRole:         in.Role,
			ConceptID:      concept.ID,
			PrimarySystem:  in.Concept.System,
			PrimaryCode:    concept.Code,
			PrimaryDisplay: concept.Display,
			Icd10ChosenBy:  signerID,
			CreatedBy:      signerID,
		}
		if std == defaultDiagnosisCodeSystem {
			// Merchant standard ICD-10 → single row, primary = ICD-10,
			// dual columns empty (spec §7.3). Without a submitted mapping
			// the resolved concept snapshot stands in (ICD-10 belum
			// terimpor — spec §7.4).
			p.PrimarySystem = defaultDiagnosisCodeSystem
			if in.Icd10 != nil {
				p.PrimaryCode = in.Icd10.Code
				if in.Icd10.Display != "" {
					p.PrimaryDisplay = in.Icd10.Display
				}
			}
		} else if in.Icd10 != nil {
			p.Icd10Code = pgtype.Text{String: in.Icd10.Code, Valid: true}
			p.Icd10Display = pgtype.Text{String: in.Icd10.Display, Valid: in.Icd10.Display != ""}
			p.Icd10Source = pgtype.Text{String: in.Icd10.Source, Valid: true}
			p.Icd10MapSet = pgtype.Text{String: in.Icd10.MapSet, Valid: in.Icd10.MapSet != ""}
			if in.Icd10.Source == "manual" {
				p.Icd10ChosenAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
			}
		}
		if _, err := q.CreateVisitDiagnosis(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
