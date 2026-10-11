package server

// Pure (DB-free) fee rule selection and fee computation for practitioner
// fees, per spec 2026-10-07-tariff-d-practitioner-fees-tax §4.1–4.2.
// The DB loader that feeds these functions is added by Task 4
// (computeFees); the functions here must stay side-effect free.

import (
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

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

// feeRuleSnapshot is the JSON snapshot of the winning rule stored with each
// operations.practitioner_fee row (spec §3 rule_snapshot): the fee must stay
// reproducible even after the rule changes or is deleted.
type feeRuleSnapshot struct {
	ID            string `json:"id"`
	Basis         string `json:"basis"`
	ComponentCode string `json:"component_code,omitempty"`
	PercentBP     int64  `json:"percent_bp,omitempty"`
	FixedCents    int64  `json:"fixed_cents,omitempty"`
	EffectiveFrom string `json:"effective_from"`
}

// parseSplitComponents decodes the components jsonb of a charge_split row
// ([{"code","amount"}], amount a 2-decimal string) into cents per code.
func parseSplitComponents(raw []byte) (map[string]int64, error) {
	out := map[string]int64{}
	if len(raw) == 0 {
		return out, nil
	}
	var rows []struct {
		Code   string `json:"code"`
		Amount string `json:"amount"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		cents, err := parseCents(r.Amount)
		if err != nil {
			return nil, err
		}
		out[r.Code] += cents
	}
	return out, nil
}

// activeFeeRules loads the merchant's fee rules as the pure feeRule view used
// by selectFeeRule/computeFee.
func activeFeeRules(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) ([]feeRule, error) {
	rows, err := q.ListFeeRulesByMerchant(c.Request.Context(), merchantID)
	if err != nil {
		return nil, err
	}
	rules := make([]feeRule, 0, len(rows))
	for _, r := range rows {
		fr := feeRule{ID: r.ID.String(), Basis: r.Basis, EffectiveFrom: r.EffectiveFrom.Time}
		if r.PhysicianID.Valid {
			fr.PhysicianID = r.PhysicianID.String()
		}
		if r.ServiceItemID.Valid {
			fr.ServiceItemID = r.ServiceItemID.String()
		}
		if r.ItemType.Valid {
			fr.ItemType = r.ItemType.String
		}
		if r.PayerID.Valid {
			fr.PayerID = r.PayerID.String()
		}
		if r.PerformerRole.Valid {
			fr.PerformerRole = r.PerformerRole.String
		}
		if r.ComponentCode.Valid {
			fr.ComponentCode = r.ComponentCode.String
		}
		if r.Percent.Valid {
			bp, err := numericToCents(r.Percent) // numeric percent "40.00" → 4000 BP
			if err != nil {
				return nil, err
			}
			fr.PercentBP = bp
		}
		if r.Amount.Valid {
			cents, err := numericToCents(r.Amount)
			if err != nil {
				return nil, err
			}
			fr.FixedCents = cents
		}
		rules = append(rules, fr)
	}
	return rules, nil
}

// computeFees snapshots the practitioner fees of a finalized account (spec
// §4.1–4.2): for every active charge line and its performers it selects the
// winning rule per pelaksana, computes the fee from the stored splits
// (combined component portions of ALL parties) and inserts one
// operations.practitioner_fee row per (charge, performer) — including zero
// fees without a matching rule (rule_id NULL, spec §4.1). Called inside the
// finalize transaction, after the receivables.
func computeFees(c *gin.Context, q *sqlcgen.Queries, acct *sqlcgen.OperationsBillingAccount) error {
	ctx := c.Request.Context()
	rules, err := activeFeeRules(c, q, acct.MerchantID)
	if err != nil {
		return err
	}
	charges, err := q.ListChargeFeeLinesForAccount(ctx, acct.ID)
	if err != nil {
		return err
	}
	perfRows, err := q.ListChargePerformersForAccount(ctx, acct.ID)
	if err != nil {
		return err
	}
	perfByCharge := map[string][]sqlcgen.ListChargePerformersForAccountRow{}
	for _, p := range perfRows {
		perfByCharge[p.ChargeID.String()] = append(perfByCharge[p.ChargeID.String()], p)
	}

	firstForVisit := map[string]bool{} // physician → already had a line in this account
	for _, ch := range charges {
		splits, err := q.ListSplitsWithComponentsForCharge(ctx, ch.ID)
		if err != nil {
			return err
		}
		comps := map[string]int64{}
		var total int64
		allCap := len(splits) > 0
		firstPayer := ""
		for _, s := range splits {
			if s.Coverage != "capitation" && s.Coverage != "package" {
				allCap = false
			}
			cents, err := numericToCents(s.Amount)
			if err != nil {
				return err
			}
			total += cents
			sc, err := parseSplitComponents(s.Components)
			if err != nil {
				return err
			}
			for code, v := range sc {
				comps[code] += v
			}
			if s.Party == "payer" && cents > 0 && firstPayer == "" {
				firstPayer = s.PayerID.String()
			}
		}

		for _, p := range perfByCharge[ch.ID.String()] {
			physicianID := p.PhysicianID.String()
			// Spec §4.2: lines whose splits are all capitation/package
			// produce no fee at all (v1).
			if allCap {
				continue
			}
			rule, found := selectFeeRule(rules, feeCtx{
				PhysicianID: physicianID, ServiceItemID: ch.ServiceItemID.String(),
				ItemType: ch.ItemType, PayerID: firstPayer, PerformerRole: p.Role,
			})
			gross, fee := computeFee(rule, feeLine{
				AccountID: acct.ID.String(), Quantity: quantityMilli(ch.Quantity),
				TotalCents: total, Components: comps,
				FirstForVisit:          !firstForVisit[physicianID],
				AllCapitationOrPackage: allCap,
			})
			var ruleID pgtype.UUID
			snapshot := []byte("{}")
			if found {
				id, err := pgtypeUUIDFromString(rule.ID)
				if err != nil {
					return err
				}
				ruleID = id
				buf, err := json.Marshal(feeRuleSnapshot{
					ID: rule.ID, Basis: rule.Basis, ComponentCode: rule.ComponentCode,
					PercentBP: rule.PercentBP, FixedCents: rule.FixedCents,
					EffectiveFrom: rule.EffectiveFrom.Format("2006-01-02"),
				})
				if err != nil {
					return err
				}
				snapshot = buf
			}
			if _, err := q.InsertPractitionerFee(ctx, sqlcgen.InsertPractitionerFeeParams{
				CompanyID: acct.CompanyID, MerchantID: acct.MerchantID, AccountID: acct.ID,
				ChargeID: ch.ID, PhysicianID: p.PhysicianID, Role: p.Role,
				ServiceDate: ch.ServiceDate, GrossAmount: centsNum(gross), FeeAmount: centsNum(fee),
				RuleID: ruleID, RuleSnapshot: snapshot,
			}); err != nil {
				return err
			}
			firstForVisit[physicianID] = true
		}
	}
	return nil
}
