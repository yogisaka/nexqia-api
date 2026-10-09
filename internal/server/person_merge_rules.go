// internal/server/person_merge_rules.go
// Pure merge rules (spec 2026-10-07-merge-medical-records §5, §6).
package server

import "sort"

// mergeIdentity is what the identity check needs from one record.
type mergeIdentity struct {
	IdentityHash string // nik_search_hash, "" = no identity number
	IdentityType string // identity_type_concept_id, "" = unknown
	BirthDate    string // YYYY-MM-DD or ""
	Gender       string
}

// identityCheck: Result ok | same_identity | needs_documents | blocked.
// Reasons/Warnings are codes the FE maps to Indonesian text.
type identityCheck struct {
	Result   string   `json:"result"`
	Reasons  []string `json:"reasons"`
	Warnings []string `json:"warnings"`
}

func checkMergeIdentity(s, m mergeIdentity) identityCheck {
	basics := s.BirthDate != "" && s.BirthDate == m.BirthDate && s.Gender != "" && s.Gender == m.Gender
	sHas, mHas := s.IdentityHash != "", m.IdentityHash != ""
	blocked := func(code string) identityCheck {
		return identityCheck{Result: "blocked", Reasons: []string{code}, Warnings: []string{}}
	}
	switch {
	case sHas && mHas && s.IdentityHash == m.IdentityHash:
		c := identityCheck{Result: "same_identity", Reasons: []string{}, Warnings: []string{}}
		if !basics {
			c.Warnings = append(c.Warnings, "same_identity_basics_differ")
		}
		return c
	case sHas && mHas && s.IdentityType == m.IdentityType:
		return blocked("identity_mismatch")
	case sHas && mHas, !sHas && !mHas:
		if !basics {
			return blocked("basics_mismatch")
		}
		return identityCheck{Result: "needs_documents", Reasons: []string{}, Warnings: []string{}}
	default:
		if !basics {
			return blocked("basics_mismatch")
		}
		return identityCheck{Result: "ok", Reasons: []string{}, Warnings: []string{}}
	}
}

// mergeFields are the pick-one person fields (spec §6); "identity" stands
// for nik + nik_search_hash + identity_type_concept_id together.
var mergeFields = []string{
	"full_name", "birth_date", "birth_place", "gender", "blood_type", "marital_status",
	"religion_concept_id", "education_concept_id", "ethnicity_concept_id", "phone", "email",
	"address", "family_id", "photo_url", "region_village_concept_id", "citizenship_concept_id",
	"job_concept_id", "marital_status_concept_id", "blood_type_concept_id", "identity",
}

// identityKey compares the identity group by its hash (ciphertext differs per encryption).
func identityKey(v map[string]string) string { return v["nik_search_hash"] }

// resolveSurvivorship returns the final survivor values, the applied choice
// per field (survivor|merged|autofill) and the conflicting fields missing a
// choice (sorted). Inputs hold every mergeFields column plus nik,
// nik_search_hash and identity_type_concept_id; "" means empty.
func resolveSurvivorship(s, m, choices map[string]string, survivorID string) (map[string]string, map[string]string, []string) {
	final := map[string]string{}
	for k, v := range s {
		final[k] = v
	}
	applied := map[string]string{}
	var missing []string
	for _, f := range mergeFields {
		var sv, mv string
		if f == "identity" {
			sv, mv = identityKey(s), identityKey(m)
		} else {
			sv, mv = s[f], m[f]
		}
		take := ""
		switch {
		case sv == mv || mv == "":
			continue
		case sv == "":
			take = "autofill"
		default:
			c := choices[f]
			if c != "survivor" && c != "merged" {
				missing = append(missing, f)
				continue
			}
			take = c
		}
		applied[f] = take
		if take == "survivor" {
			continue
		}
		if f == "identity" {
			final["nik"], final["nik_search_hash"], final["identity_type_concept_id"] = m["nik"], m["nik_search_hash"], m["identity_type_concept_id"]
		} else {
			final[f] = mv
		}
	}
	if final["family_id"] == survivorID {
		final["family_id"] = ""
	}
	sort.Strings(missing)
	return final, applied, missing
}
