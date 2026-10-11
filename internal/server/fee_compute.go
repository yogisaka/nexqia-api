package server

// Pure (DB-free) fee rule selection and fee computation for practitioner
// fees, per spec 2026-10-07-tariff-d-practitioner-fees-tax §4.1–4.2.
// The DB loader that feeds these functions is added by Task 4
// (computeFees); the functions here must stay side-effect free.

import "time"

// feeRule is the pure, DB-free view of core.practitioner_fee_rule used by
// selectFeeRule/computeFee. Empty string means "no constraint" (NULL in the
// table). Money is in cents; percentages in basis points (100 = 1%).
type feeRule struct {
	ID            string
	PhysicianID   string // "" = all physicians
	ServiceItemID string // "" = all items
	ItemType      string // "" = all item types
	PayerID       string // "" = all payers (incl. general patients)
	PerformerRole string // "" = all roles
	Basis         string // percent_of_component | percent_of_total | fixed_per_item | fixed_per_visit
	ComponentCode string // required when Basis = percent_of_component
	PercentBP     int64  // for percent_* bases
	FixedCents    int64  // for fixed_* bases
	EffectiveFrom time.Time
}

// feeCtx identifies the line context a rule is selected for.
type feeCtx struct {
	PhysicianID   string
	ServiceItemID string
	ItemType      string
	PayerID       string // "" = general patient (first responsible payer)
	PerformerRole string
}

// feeLine is the charge line a fee is computed from. Components holds the
// combined portions of ALL parties of the line per component code.
type feeLine struct {
	AccountID     string
	Quantity      int64
	TotalCents    int64            // total of all parties
	Components    map[string]int64 // combined component portions, cents
	FirstForVisit bool             // true only for the first line of (account, physician)
	// AllCapitationOrPackage is true when every split of the line has
	// coverage capitation or package: the line produces no fee (spec §4.2).
	AllCapitationOrPackage bool
}

// selectFeeRule picks the winning rule among active candidates per spec §4.1:
// specificity base score 30 (physician) + 20 (item) + 10 (item_type), plus 2
// when the rule constrains the payer and it matches, plus 1 when the rule
// constrains the performer role and it matches. Highest score wins; ties go
// to the newest effective_from, then the smallest id.
func selectFeeRule(rules []feeRule, ctx feeCtx) (feeRule, bool) {
	var best feeRule
	bestScore := int64(0)
	found := false
	for _, r := range rules {
		if !feeRuleMatches(r, ctx) {
			continue
		}
		score := int64(0)
		if r.PhysicianID != "" {
			score += 30
		}
		if r.ServiceItemID != "" {
			score += 20
		}
		if r.ItemType != "" {
			score += 10
		}
		if r.PayerID != "" && r.PayerID == ctx.PayerID {
			score += 2
		}
		if r.PerformerRole != "" && r.PerformerRole == ctx.PerformerRole {
			score += 1
		}
		if !found || score > bestScore ||
			(score == bestScore &&
				(r.EffectiveFrom.After(best.EffectiveFrom) ||
					(r.EffectiveFrom.Equal(best.EffectiveFrom) && r.ID < best.ID))) {
			best, bestScore, found = r, score, true
		}
	}
	return best, found
}

// feeRuleMatches reports whether an active rule is a candidate for ctx: every
// constrained dimension is either open ("") or equal to the context value.
func feeRuleMatches(r feeRule, ctx feeCtx) bool {
	return (r.PhysicianID == "" || r.PhysicianID == ctx.PhysicianID) &&
		(r.ServiceItemID == "" || r.ServiceItemID == ctx.ServiceItemID) &&
		(r.ItemType == "" || r.ItemType == ctx.ItemType) &&
		(r.PayerID == "" || r.PayerID == ctx.PayerID) &&
		(r.PerformerRole == "" || r.PerformerRole == ctx.PerformerRole)
}

// computeFee returns the line's gross amount (MEDIS portion of all parties
// when present, else the line total) and the practitioner fee for the chosen
// rule, per spec §4.2. Percent results are rounded half-up to the cent.
// fixed_per_visit applies only once per (account, physician); lines whose
// splits are all capitation/package produce no fee.
func computeFee(rule feeRule, line feeLine) (grossCents, feeCents int64) {
	if v, ok := line.Components["MEDIS"]; ok {
		grossCents = v
	} else {
		grossCents = line.TotalCents
	}
	if line.AllCapitationOrPackage {
		return grossCents, 0
	}
	switch rule.Basis {
	case "percent_of_component":
		feeCents = halfUpCents(line.Components[rule.ComponentCode]*rule.PercentBP, 10000)
	case "percent_of_total":
		feeCents = halfUpCents(line.TotalCents*rule.PercentBP, 10000)
	case "fixed_per_item":
		feeCents = rule.FixedCents * line.Quantity
	case "fixed_per_visit":
		if line.FirstForVisit {
			feeCents = rule.FixedCents
		}
	}
	return grossCents, feeCents
}

// halfUpCents divides num/den rounding half up (both positive).
func halfUpCents(num, den int64) int64 {
	return (num + den/2) / den
}
