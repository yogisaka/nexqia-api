// internal/server/billing_account.go
// Billing account API (spec 2026-10-07-tariff-b1-billing-core §5/§6): charge
// rows per visit, void, out-of-benefit consent, finalize and reopen, plus the
// DB-backed recompute that feeds the pure split engine (billing_split.go).
package server

import (
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Permissions (spec §3.14). queue_flow_presets.go keeps its own unexported
// permInvoiceCreate constant for flow presets; the endpoint guards here use
// these exported ones.
const (
	PermInvoiceCreate = "billing.invoice.create"
	PermInvoiceView   = "billing.invoice.view"
	PermAccountReopen = "billing.account.reopen"
)

// errAlreadyResponded marks a helper that already wrote the HTTP response
// (hetCentsOn); the caller must stop without writing again.
var errAlreadyResponded = errors.New("response already written")

// RegisterBillingRoutes wires the visit billing endpoints (spec §6).
func RegisterBillingRoutes(rg *gin.RouterGroup) {
	rg.GET("/admissions/:id/billing", AccessLog("billing", "view", "id"), GetAdmissionBillingHandler)
	rg.POST("/admissions/:id/charges", CreateChargeHandler)
	rg.POST("/charges/:id/void", VoidChargeHandler)
	rg.POST("/admissions/:id/billing/consents", CreateBillingConsentHandler)
	rg.POST("/admissions/:id/billing/finalize", FinalizeBillingHandler)
	rg.POST("/admissions/:id/billing/reopen", ReopenBillingHandler)
}

// parseQuantity3 parses a positive decimal string with at most 3 fraction
// digits ("2", "1.5", "0.125") into milli-units for numeric(12,3).
func parseQuantity3(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if len(frac) > 3 || (hasDot && frac == "") {
		return 0, false
	}
	if whole == "" {
		whole = "0"
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	milli, err := strconv.ParseInt(whole+frac+strings.Repeat("0", 3-len(frac)), 10, 64)
	if err != nil || milli <= 0 {
		return 0, false
	}
	return milli, true
}

// numStr3 renders a numeric(12,3) quantity as a decimal string ("1.500").
func numStr3(n pgtype.Numeric) string {
	if !n.Valid || n.Int == nil {
		return "0.000"
	}
	v := new(big.Int).Set(n.Int)
	exp := n.Exp + 3
	ten := big.NewInt(10)
	for ; exp > 0; exp-- {
		v.Mul(v, ten)
	}
	for ; exp < 0; exp++ {
		v.Quo(v, ten)
	}
	neg := ""
	if v.Sign() < 0 {
		neg, v = "-", new(big.Int).Neg(v)
	}
	s := v.String()
	for len(s) < 4 {
		s = "0" + s
	}
	return neg + s[:len(s)-3] + "." + s[len(s)-3:]
}

// centsNum converts cents to an exact numeric(15,2) (same shape as
// centsToNumeric; local alias keeps the split wiring readable).
func centsNum(c int64) pgtype.Numeric { return centsToNumeric(c) }

// glStatus reports, per plan-requires-GL payer that covers the visit's care
// type, whether a live guarantee letter is valid on every active charge's
// service date.
type glStatus struct {
	PayerID string
	Name    string
	OK      bool
}

// recomputeResult carries the engine output plus the GL summary needed by the
// finalize guard.
type recomputeResult struct {
	split splitResult
	gl    []glStatus
}

// priceForBilling resolves one unit price (cents, HET-capped) of an item on a
// price list valid at date, using the same queries as loadPriceResolveContext
// (price_list.go) but without writing any HTTP response. Inactive list/base or
// no resolvable price → ok=false; DB errors return err (or errAlreadyResponded
// when hetCentsOn already responded).
func priceForBilling(c *gin.Context, q *sqlcgen.Queries, listID string, item sqlcgen.CoreServiceItem, date pgtype.Date) (int64, bool, error) {
	if listID == "" {
		return 0, false, nil
	}
	id, err := pgtypeUUIDFromString(listID)
	if err != nil {
		return 0, false, nil
	}
	list, err := q.GetPriceListByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !list.IsActive {
		return 0, false, nil
	}
	rule := priceListRule{}
	var baseRows []sqlcgen.ListEffectiveRatesForListRow
	if list.BasePriceListID.Valid {
		base, err := q.GetPriceListByID(c.Request.Context(), list.BasePriceListID)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if !base.IsActive {
			return 0, false, nil
		}
		rule.Derived = true
		adjustBP, err := numericToCents(list.AdjustmentPercent)
		if err != nil {
			return 0, false, err
		}
		rule.AdjustmentBP = adjustBP
		rounding, err := numericToCents(list.RoundingUnit)
		if err != nil {
			return 0, false, err
		}
		rule.RoundingCents = rounding
		adjustments, err := q.ListPriceListAdjustments(c.Request.Context(), list.ID)
		if err != nil {
			return 0, false, err
		}
		rule.CategoryAdjustmentBP = make(map[string]int64, len(adjustments))
		for _, a := range adjustments {
			bp, err := numericToCents(a.AdjustmentPercent)
			if err != nil {
				return 0, false, err
			}
			rule.CategoryAdjustmentBP[a.ItemType] = bp
		}
		rows, err := q.ListEffectiveRatesForList(c.Request.Context(), sqlcgen.ListEffectiveRatesForListParams{
			PriceListID: base.ID, OnDate: date,
		})
		if err != nil {
			return 0, false, err
		}
		baseRows = rows
	}
	own, err := q.ListEffectiveRatesForItem(c.Request.Context(), sqlcgen.ListEffectiveRatesForItemParams{
		PriceListID: list.ID, ServiceItemID: item.ID, OnDate: date,
	})
	if err != nil {
		return 0, false, err
	}
	toRows := func(rows []sqlcgen.ListEffectiveRatesForListRow, wantItem pgtype.UUID) ([]rateRow, error) {
		out := make([]rateRow, 0, len(rows))
		for _, r := range rows {
			if wantItem.Valid && r.ServiceItemID != wantItem {
				continue
			}
			cents, err := numericToCents(r.Amount)
			if err != nil {
				return nil, err
			}
			out = append(out, rateRow{ComponentID: r.RateComponentID.String(), Code: r.Code, Name: r.Name, Cents: cents})
		}
		return out, nil
	}
	ownRows := make([]rateRow, 0, len(own))
	for _, r := range own {
		cents, err := numericToCents(r.Amount)
		if err != nil {
			return 0, false, err
		}
		ownRows = append(ownRows, rateRow{ComponentID: r.RateComponentID.String(), Code: r.Code, Name: r.Name, Cents: cents})
	}
	baseFiltered, err := toRows(baseRows, item.ID)
	if err != nil {
		return 0, false, err
	}
	res, err := resolvePrice(item.ItemType, rule, ownRows, baseFiltered)
	if err != nil {
		return 0, false, nil // includes errPriceMissing
	}
	hetCents, okHET := hetCentsOn(c, q, item.ID, date)
	if !okHET {
		return 0, false, errAlreadyResponded
	}
	res = capAtHET(res, hetCents)
	return res.TotalCents, true, nil
}

// pgtypeUUIDFromString parses a UUID string ("" handled by callers).
func pgtypeUUIDFromString(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return id, nil
}

// recomputeAccount locks the account row, rebuilds the split-engine inputs
// (charges, guarantors, plan rules, GL, coverage, annual usage, facility
// profile), runs splitAccount and replaces the account's charge_split rows
// with the fresh result (spec §4/§5). Caller must have verified the account is
// open; the account row lock makes the read-modify-write atomic.
func recomputeAccount(c *gin.Context, q *sqlcgen.Queries, account sqlcgen.OperationsBillingAccount) (recomputeResult, error) {
	ctx := c.Request.Context()
	out := recomputeResult{}

	locked, err := q.GetBillingAccountForUpdate(ctx, account.ID)
	if err != nil {
		return out, err
	}
	if locked.Status != "open" {
		// Frozen account: nothing to recompute.
		return out, nil
	}
	adm, err := q.GetAdmissionByID(ctx, locked.AdmissionID)
	if err != nil {
		return out, err
	}
	merchant, err := q.GetMerchantByID(ctx, locked.MerchantID)
	if err != nil {
		return out, err
	}
	charges, err := q.ListActiveChargesByAccount(ctx, locked.ID)
	if err != nil {
		return out, err
	}
	guarantors, err := q.ListAdmissionGuarantors(ctx, locked.AdmissionID)
	if err != nil {
		return out, err
	}

	// Items referenced by the charges (validated active + merchant-owned at
	// insert; load once for the price lookups).
	items := make(map[string]sqlcgen.CoreServiceItem, len(charges))
	engineCharges := make([]splitCharge, 0, len(charges))
	for _, ch := range charges {
		itemID := ch.ServiceItemID.String()
		if _, ok := items[itemID]; !ok {
			item, err := q.GetServiceItemByID(ctx, ch.ServiceItemID)
			if err != nil {
				return out, err
			}
			items[itemID] = item
		}
		engineCharges = append(engineCharges, splitCharge{
			ID: ch.ID.String(), ItemID: itemID, ItemType: items[itemID].ItemType,
			QtyMilli:    quantityMilli(ch.Quantity),
			ServiceDate: ch.ServiceDate.Time,
		})
	}

	defaultList := ""
	if merchant.DefaultPriceListID.Valid {
		defaultList = merchant.DefaultPriceListID.String()
	}

	// Facility BPJS profile → default coverage for BPJS payers (spec §4.2).
	defaultCoverage := ""
	switch merchant.BpjsCooperation {
	case "fktp_capitation":
		defaultCoverage = "capitation"
	case "fktp_network":
		defaultCoverage = "excluded"
	case "fkrtl":
		defaultCoverage = "package"
	}

	// Merchant-local "now" for the plan-year window.
	loc, err := merchantLocation(ctx, q, locked.MerchantID)
	if err != nil {
		return out, err
	}
	nowLocal := localDayStart(time.Now(), loc)

	payers := make([]splitPayer, 0, len(guarantors))
	for _, g := range guarantors {
		payer, err := q.GetPayerByID(ctx, g.PayerID)
		if err != nil {
			return out, err
		}
		priceList := defaultList
		if payer.PriceListID.Valid {
			priceList = payer.PriceListID.String()
		}
		sp := splitPayer{
			PayerID: payer.ID.String(), PayerType: payer.PayerType, PriceListID: priceList,
			Coverage: map[string]bpjsCoverage{}, Blacklist: map[string]bool{},
			DiscountBP: map[string]int64{}, DiscountCents: map[string]int64{},
			InnerLimitCents:      map[string]int64{},
			AnnualRemainingCents: -1, CopayMaxCents: -1,
		}
		if payer.PayerType == "bpjs" {
			sp.DefaultCoverage = defaultCoverage
			covs, err := q.ListPayerItemCoverage(ctx, payer.ID)
			if err != nil {
				return out, err
			}
			for _, cov := range covs {
				claimType := ""
				if cov.ClaimType.Valid {
					claimType = cov.ClaimType.String
				}
				if cov.ServiceItemID.Valid {
					sp.Coverage["item:"+cov.ServiceItemID.String()] = bpjsCoverage{Coverage: cov.Coverage, ClaimType: claimType}
				} else if cov.ItemType.Valid {
					sp.Coverage["type:"+cov.ItemType.String] = bpjsCoverage{Coverage: cov.Coverage, ClaimType: claimType}
				}
			}
		}
		// Service rules: blacklist items and contract discounts.
		rules, err := q.ListPayerServiceRules(ctx, payer.ID)
		if err != nil {
			return out, err
		}
		for _, r := range rules {
			if !r.IsActive {
				continue
			}
			key := r.ServiceItemID.String()
			switch r.RuleType {
			case "blacklist":
				sp.Blacklist[key] = true
			case "discount":
				if r.DiscountPercent.Valid {
					if bp, err := numericToCents(r.DiscountPercent); err == nil && bp > 0 {
						sp.DiscountBP[key] = bp
					}
				}
				if r.DiscountAmount.Valid {
					if cents, err := numericToCents(r.DiscountAmount); err == nil && cents > 0 {
						sp.DiscountCents[key] = cents
					}
				}
			}
		}
		// Plan: rule for the visit's care type, inner limits, annual usage, GL.
		planID := g.PlanID
		var tpaRemaining pgtype.Numeric
		if g.CoverageID.Valid {
			cov, err := q.GetPatientCoverageByID(ctx, g.CoverageID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return out, err
			}
			if err == nil {
				if !planID.Valid {
					planID = cov.PlanID
				}
				tpaRemaining = cov.TpaRemainingAnnual
			}
		}
		if planID.Valid {
			plan, err := q.GetPayerPlanByID(ctx, planID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return out, err
			}
			if err == nil && plan.IsActive && plan.PayerID == payer.ID {
				rules, err := q.ListPlanRules(ctx, plan.ID)
				if err != nil {
					return out, err
				}
				for _, r := range rules {
					if r.CareType != adm.AdmissionType {
						continue
					}
					sp.HasPlanRule = true
					if copay, err := numericToCents(r.CopayPercent); err == nil {
						sp.CopayBP = copay
					}
					if max, err := numericToCents(r.CopayMaxPerClaim); err == nil && r.CopayMaxPerClaim.Valid {
						sp.CopayMaxCents = max
					}
					if ded, err := numericToCents(r.DeductiblePerClaim); err == nil {
						sp.DeductibleCents = ded
					}
					if vl, err := numericToCents(r.VisitLimit); err == nil && r.VisitLimit.Valid {
						sp.VisitLimitCents = vl
					}
				}
				limits, err := q.ListPlanLimits(ctx, plan.ID)
				if err != nil {
					return out, err
				}
				for _, l := range limits {
					if cents, err := numericToCents(l.LimitPerVisit); err == nil {
						sp.InnerLimitCents[l.ItemType] = cents
					}
				}
				if plan.RequiresGl {
					sp.RequiresGL = true
					letters, err := q.ListGuaranteeLetters(ctx, locked.AdmissionID)
					if err != nil {
						return out, err
					}
					for _, gl := range letters {
						if gl.PayerID != payer.ID {
							continue
						}
						glCents, err := numericToCents(gl.AmountLimit)
						if err != nil {
							return out, err
						}
						if glCents > sp.GLCents {
							sp.GLCents = glCents
						}
					}
					from, to := guaranteeWindow(letters, payer.ID)
					sp.GLValidOn = func(t time.Time) bool { return glCovers(from, to, t) }
				}
				if plan.AnnualLimit.Valid {
					limitCents, err := numericToCents(plan.AnnualLimit)
					if err != nil {
						return out, err
					}
					start, end := planYearWindow(nowLocal, plan.PlanYearStartMonth)
					row, err := q.SumPayerAnnualUsage(ctx, sqlcgen.SumPayerAnnualUsageParams{
						PayerID: payer.ID, PersonID: adm.PersonID, AdmissionID: locked.AdmissionID,
						ComputedAt:   pgtype.Timestamptz{Time: start, Valid: true},
						ComputedAt_2: pgtype.Timestamptz{Time: end, Valid: true},
						PlanID:       plan.ID,
					})
					if err != nil {
						return out, err
					}
					used, err := numericToCents(row)
					if err != nil {
						return out, err
					}
					remaining := limitCents - used
					if tpaRemaining.Valid {
						if tpa, err := numericToCents(tpaRemaining); err == nil && tpa < remaining {
							remaining = tpa
						}
					}
					if remaining < 0 {
						remaining = 0
					}
					sp.AnnualRemainingCents = remaining
				}
			}
		}
		payers = append(payers, sp)
	}

	// Price lookup with a per-recompute cache keyed by (list, item, day).
	type priceKey struct {
		list, item string
		day        string
	}
	cache := map[priceKey]int64{}
	priceFn := func(listID, itemID string, on time.Time) (int64, bool, error) {
		key := priceKey{listID, itemID, on.Format("2006-01-02")}
		if cents, ok := cache[key]; ok {
			return cents, true, nil
		}
		cents, ok, err := priceForBilling(c, q, listID, items[itemID], pgtype.Date{Time: on, Valid: true})
		if err != nil || !ok {
			return 0, false, err
		}
		cache[key] = cents
		return cents, true, nil
	}

	res, err := splitAccount(engineCharges, payers, defaultList, priceFn)
	if err != nil {
		return out, err
	}

	// Replace the account's splits with the fresh result.
	if err := q.DeleteChargeSplitsByAccount(ctx, locked.ID); err != nil {
		return out, err
	}
	for _, ch := range engineCharges {
		seq := int16(0)
		chargeUUID, err := pgtypeUUIDFromString(ch.ID)
		if err != nil {
			return out, err
		}
		for _, part := range res.ByCharge[ch.ID] {
			seq++
			var payerID pgtype.UUID
			if part.PayerID != "" {
				payerID, err = pgtypeUUIDFromString(part.PayerID)
				if err != nil {
					return out, err
				}
			}
			listID, err := pgtypeUUIDFromString(part.PriceListID)
			if err != nil {
				return out, err
			}
			var shadow pgtype.Numeric
			if part.HasShadow {
				shadow = centsNum(part.ShadowCents)
			}
			// The engine leaves the patient remainder's coverage empty when a
			// guarantor exists but does not bear the remainder; the DB enum
			// (and spec §4) names that case "uncovered".
			coverage := part.Coverage
			if coverage == "" {
				coverage = "uncovered"
			}
			if err := q.InsertChargeSplit(ctx, sqlcgen.InsertChargeSplitParams{
				CompanyID: locked.CompanyID, MerchantID: locked.MerchantID, ChargeID: chargeUUID,
				Seq: seq, Party: part.Party, PayerID: payerID, Coverage: coverage,
				ClaimType:   pgtype.Text{String: part.ClaimType, Valid: part.ClaimType != ""},
				PriceListID: listID, UnitAmount: centsNum(part.UnitCents), GrossAmount: centsNum(part.GrossCents),
				DiscountAmount: centsNum(part.DiscountCents), Amount: centsNum(part.AmountCents),
				ShadowAmount: shadow, Note: pgtype.Text{String: part.Note, Valid: part.Note != ""},
			}); err != nil {
				return out, err
			}
		}
	}

	// GL summary: plan-requires-GL payers whose plan rule covers this care
	// type must have a valid letter on every active charge date.
	for i := range payers {
		p := payers[i]
		if !p.RequiresGL || !p.HasPlanRule {
			continue
		}
		ok := true
		for _, ch := range engineCharges {
			if p.GLValidOn != nil && !p.GLValidOn(ch.ServiceDate) {
				ok = false
				break
			}
		}
		name := p.PayerID
		if payer, err := q.GetPayerByID(ctx, pgtypeMustUUID(p.PayerID)); err == nil {
			name = payer.Name
		}
		out.gl = append(out.gl, glStatus{PayerID: p.PayerID, Name: name, OK: ok})
	}
	out.split = res
	return out, nil
}

// quantityMilli reads a numeric(12,3) quantity back as milli-units.
func quantityMilli(n pgtype.Numeric) int64 {
	if !n.Valid || n.Int == nil {
		return 0
	}
	v := new(big.Int).Set(n.Int)
	exp := n.Exp + 3
	ten := big.NewInt(10)
	for ; exp > 0; exp-- {
		v.Mul(v, ten)
	}
	for ; exp < 0; exp++ {
		v.Quo(v, ten)
	}
	if !v.IsInt64() {
		return 0
	}
	return v.Int64()
}

// guaranteeWindow returns the widest valid_from / valid_to of a payer's live
// letters (valid_to zero time = open end).
func guaranteeWindow(letters []sqlcgen.OperationsGuaranteeLetter, payerID pgtype.UUID) (time.Time, time.Time) {
	var from, to time.Time
	for _, gl := range letters {
		if gl.PayerID != payerID {
			continue
		}
		if from.IsZero() || gl.ValidFrom.Time.Before(from) {
			from = gl.ValidFrom.Time
		}
		if gl.ValidTo.Valid && (to.IsZero() || gl.ValidTo.Time.After(to)) {
			to = gl.ValidTo.Time
		}
	}
	return from, to
}

// glCovers reports whether some letter is valid on t.
func glCovers(from, to, t time.Time) bool {
	if from.IsZero() || t.Before(from) {
		return false
	}
	if to.IsZero() {
		return true
	}
	day := t.Format("2006-01-02")
	return !dayAfter(day, to.Format("2006-01-02"))
}

func dayAfter(a, b string) bool { return a > b }

// planYearWindow returns the [start, end) of the plan's running year that
// contains now, counting from plan_year_start_month (1-12).
func planYearWindow(now time.Time, startMonth int16) (time.Time, time.Time) {
	start := time.Date(now.Year(), time.Month(startMonth), 1, 0, 0, 0, 0, now.Location())
	if start.After(now) {
		start = start.AddDate(-1, 0, 0)
	}
	return start, start.AddDate(1, 0, 0)
}

// pgtypeMustUUID is a best-effort parse for ids the engine produced from
// validated DB uuids.
func pgtypeMustUUID(s string) pgtype.UUID {
	id, _ := pgtypeUUIDFromString(s)
	return id
}

// billingPayload renders the GET /admissions/:id/billing body from the stored
// rows: account header, guarantors, charges with their splits, totals and the
// consent state. needs_consent lists BPJS-excluded charges that still lack an
// applicable consent (per charge or blanket).
func billingPayload(c *gin.Context, q *sqlcgen.Queries, admissionID pgtype.UUID, account *sqlcgen.OperationsBillingAccount) (gin.H, error) {
	ctx := c.Request.Context()
	guarantors, err := q.ListAdmissionGuarantors(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	charges, err := q.ListChargesForAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	splits, err := q.ListSplitsForAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	consents, err := q.ListBillingConsentsByAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}

	payerNames := map[string]string{}
	for _, g := range guarantors {
		payer, err := q.GetPayerByID(ctx, g.PayerID)
		if err == nil {
			payerNames[g.PayerID.String()] = payer.Name
		}
	}
	splitsByCharge := map[string][]gin.H{}
	payerTotals := map[string]int64{}
	var patientTotal, payerTotal int64
	excludedCandidate := map[string]bool{}
	payerCarried := map[string]bool{}
	for _, s := range splits {
		var payer any
		if s.PayerID.Valid {
			name := payerNames[s.PayerID.String()]
			payer = gin.H{"id": s.PayerID.String(), "name": name}
		}
		var shadow any
		if s.ShadowAmount.Valid {
			shadow = numStr2(s.ShadowAmount)
		}
		splitsByCharge[s.ChargeID.String()] = append(splitsByCharge[s.ChargeID.String()], gin.H{
			"party":           s.Party,
			"payer":           payer,
			"coverage":        s.Coverage,
			"claim_type":      textJSON(s.ClaimType),
			"unit_amount":     numStr2(s.UnitAmount),
			"gross_amount":    numStr2(s.GrossAmount),
			"discount_amount": numStr2(s.DiscountAmount),
			"amount":          numStr2(s.Amount),
			"shadow_amount":   shadow,
			"note":            textJSON(s.Note),
		})
		cents, err := numericToCents(s.Amount)
		if err != nil {
			return nil, err
		}
		if s.Party == "patient" {
			patientTotal += cents
			if s.Coverage == "excluded" {
				excludedCandidate[s.ChargeID.String()] = true
			}
		} else {
			payerTotal += cents
			if cents > 0 {
				payerCarried[s.ChargeID.String()] = true
			}
			if s.PayerID.Valid {
				payerTotals[s.PayerID.String()] += cents
			}
		}
	}

	var excludedCharges []string
	for _, ch := range charges {
		if excludedCandidate[ch.ID.String()] && !payerCarried[ch.ID.String()] {
			excludedCharges = append(excludedCharges, ch.ID.String())
		}
	}

	chargeJSON := make([]gin.H, 0, len(charges))
	for _, ch := range charges {
		splitRows := splitsByCharge[ch.ID.String()]
		if splitRows == nil {
			splitRows = []gin.H{}
		}
		chargeJSON = append(chargeJSON, gin.H{
			"id": ch.ID.String(),
			"service_item": gin.H{
				"id": ch.ServiceItemID.String(), "code": ch.ItemCode,
				"name": ch.ItemName, "item_type": ch.ItemType,
			},
			"quantity":     numStr3(ch.Quantity),
			"service_date": dateOrNil(ch.ServiceDate),
			"status":       ch.Status,
			"note":         textJSON(ch.Note),
			"splits":       splitRows,
		})
	}

	byPayer := make([]gin.H, 0, len(payerTotals))
	for id, cents := range payerTotals {
		byPayer = append(byPayer, gin.H{"payer_id": id, "name": payerNames[id], "amount": formatCents(cents)})
	}

	// needs_consent: excluded charges minus consented ones (per charge or
	// blanket consent without charge_id).
	blanketConsent := false
	consented := map[string]bool{}
	consentJSON := make([]gin.H, 0, len(consents))
	for _, cs := range consents {
		if !cs.ChargeID.Valid {
			blanketConsent = true
		} else {
			consented[cs.ChargeID.String()] = true
		}
		consentJSON = append(consentJSON, gin.H{
			"id": cs.ID.String(),
			"charge_id": func() any {
				if !cs.ChargeID.Valid {
					return nil
				}
				return cs.ChargeID.String()
			}(),
			"kind":              cs.Kind,
			"consented_by_name": cs.ConsentedByName,
			"relation":          cs.Relation,
			"recorded_by":       uuidJSON(cs.RecordedBy),
			"recorded_at":       tsJSON(cs.RecordedAt),
		})
	}
	needs := []string{}
	if !blanketConsent {
		for _, id := range excludedCharges {
			if !consented[id] {
				needs = append(needs, id)
			}
		}
	}

	var acctJSON any
	if account != nil {
		var patientAny, payerAny any
		if account.PatientTotal.Valid {
			patientAny = numStr2(account.PatientTotal)
		}
		if account.PayerTotal.Valid {
			payerAny = numStr2(account.PayerTotal)
		}
		acctJSON = gin.H{
			"id":             account.ID.String(),
			"status":         account.Status,
			"finalized_at":   tsJSON(account.FinalizedAt),
			"reopened_count": account.ReopenedCount,
			"patient_total":  patientAny,
			"payer_total":    payerAny,
			"row_version":    account.RowVersion,
		}
	}

	return gin.H{
		"account":    acctJSON,
		"guarantors": guarantorRowsJSON(guarantors),
		"charges":    chargeJSON,
		"totals": gin.H{
			"patient":  formatCents(patientTotal),
			"payer":    formatCents(payerTotal),
			"by_payer": byPayer,
		},
		"needs_consent": needs,
		"consents":      consentJSON,
	}, nil
}

// loadBillingAdmission resolves the admission path param with the merchant
// header and one of the given permissions.
func loadBillingAdmission(c *gin.Context, rawID string, q *sqlcgen.Queries, perms ...string) (sqlcgen.OperationsAdmission, pgtype.UUID, bool) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if !RequireAnyPermissionForMerchant(c, merchantID, perms...) {
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	admissionID, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	adm, err := q.GetAdmissionByID(c.Request.Context(), admissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	return adm, merchantID, true
}

// GetAdmissionBillingHandler godoc
// @Summary Get the billing account of a visit (recomputed when open)
// @Tags billing
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id}/billing [get]
func GetAdmissionBillingHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, _, ok := loadBillingAdmission(c, c.Param("id"), q, PermInvoiceView, PermInvoiceCreate)
	if !ok {
		return
	}
	acct, err := q.GetBillingAccountByAdmission(c.Request.Context(), adm.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	var acctPtr *sqlcgen.OperationsBillingAccount
	if err == nil {
		acctPtr = &acct
		// An open account is always shown with the freshest split (spec §5.3).
		if acct.Status == "open" {
			if _, err := recomputeAccount(c, q, acct); err != nil {
				if errors.Is(err, errAlreadyResponded) {
					return
				}
				if errors.Is(err, errPriceMissing) {
					c.JSON(http.StatusConflict, gin.H{"error": "price_missing"})
					return
				}
				respondInternalError(c, err)
				return
			}
		}
	}
	payload, err := billingPayload(c, q, adm.ID, acctPtr)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
}

type createChargeRequest struct {
	ServiceItemID string `json:"service_item_id" binding:"required"`
	Quantity      string `json:"quantity" binding:"required"`
	ServiceDate   string `json:"service_date"`
	Note          string `json:"note"`
}

// CreateChargeHandler godoc
// @Summary Add a charge row to a visit (recomputes the billing account)
// @Tags billing
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body createChargeRequest true "Charge data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /admissions/{id}/charges [post]
func CreateChargeHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, _, ok := loadBillingAdmission(c, c.Param("id"), q, PermInvoiceCreate)
	if !ok {
		return
	}
	if adm.Status == "cancelled" {
		c.JSON(http.StatusConflict, gin.H{"error": "admission is cancelled"})
		return
	}
	var req createChargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	itemID, okID := parseUUID(req.ServiceItemID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_item_id"})
		return
	}
	item, err := q.GetServiceItemByID(c.Request.Context(), itemID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (item.MerchantID != adm.MerchantID || !item.IsActive)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	milli, okQty := parseQuantity3(req.Quantity)
	if !okQty {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
		return
	}
	// Service date defaults to today in the merchant's timezone.
	loc, err := merchantLocation(c.Request.Context(), q, adm.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	day := localDayStart(time.Now(), loc)
	if req.ServiceDate != "" {
		t, okDate := parseAPIDate(req.ServiceDate)
		if !okDate {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_date"})
			return
		}
		day = t
	}

	// Account is created on the first charge; a concurrent first charge finds
	// the winner's row via the FOR UPDATE read (spec §5.1).
	if err := q.InsertBillingAccountIfAbsent(c.Request.Context(), sqlcgen.InsertBillingAccountIfAbsentParams{
		CompanyID: adm.CompanyID, MerchantID: adm.MerchantID, AdmissionID: adm.ID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	acct, err := q.GetBillingAccountByAdmission(c.Request.Context(), adm.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	acct, err = q.GetBillingAccountForUpdate(c.Request.Context(), acct.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if acct.Status != "open" {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is finalized"})
		return
	}
	if _, err := q.InsertCharge(c.Request.Context(), sqlcgen.InsertChargeParams{
		CompanyID: adm.CompanyID, MerchantID: adm.MerchantID, AccountID: acct.ID, AdmissionID: adm.ID,
		ServiceItemID: itemID, Quantity: pgtype.Numeric{Int: big.NewInt(milli), Exp: -3, Valid: true},
		ServiceDate: pgtype.Date{Time: day, Valid: true}, Note: optText(req.Note), CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := recomputeAccount(c, q, acct); err != nil {
		if errors.Is(err, errAlreadyResponded) {
			return
		}
		if errors.Is(err, errPriceMissing) {
			c.JSON(http.StatusConflict, gin.H{"error": "price_missing", "service_item_id": req.ServiceItemID})
			return
		}
		respondInternalError(c, err)
		return
	}
	payload, err := billingPayload(c, q, adm.ID, &acct)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": payload, "meta": gin.H{}})
}

// VoidChargeHandler godoc
// @Summary Void an active charge row (recomputes the billing account)
// @Tags billing
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Charge UUID"
// @Param request body voidChargeRequest true "Void reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /charges/{id}/void [post]
func VoidChargeHandler(c *gin.Context) {
	var req voidChargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	chargeID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	charge, err := q.GetChargeByID(c.Request.Context(), chargeID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "charge not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermInvoiceCreate, charge.MerchantID) {
		return
	}
	acct, err := q.GetBillingAccountForUpdate(c.Request.Context(), charge.AccountID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if acct.Status != "open" {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is finalized"})
		return
	}
	if _, err := q.VoidCharge(c.Request.Context(), sqlcgen.VoidChargeParams{
		ID: charge.ID, VoidReason: optText(req.Reason), VoidedBy: AuthUserID(c),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "charge is already void"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if _, err := recomputeAccount(c, q, acct); err != nil {
		if errors.Is(err, errAlreadyResponded) {
			return
		}
		if errors.Is(err, errPriceMissing) {
			c.JSON(http.StatusConflict, gin.H{"error": "price_missing"})
			return
		}
		respondInternalError(c, err)
		return
	}
	payload, err := billingPayload(c, q, charge.AdmissionID, &acct)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
}

type voidChargeRequest struct {
	Reason string `json:"reason" binding:"required"`
}

type billingConsentRequest struct {
	ChargeID        string `json:"charge_id"`
	ConsentedByName string `json:"consented_by_name" binding:"required"`
	Relation        string `json:"relation" binding:"required"`
}

// CreateBillingConsentHandler godoc
// @Summary Record an out-of-benefit consent (bpjs_excluded)
// @Tags billing
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body billingConsentRequest true "Consent data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id}/billing/consents [post]
func CreateBillingConsentHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, _, ok := loadBillingAdmission(c, c.Param("id"), q, PermInvoiceCreate)
	if !ok {
		return
	}
	var req billingConsentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Relation != "self" && req.Relation != "family" && req.Relation != "guardian" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid relation"})
		return
	}
	var chargeID pgtype.UUID
	if req.ChargeID != "" {
		id, okID := parseUUID(req.ChargeID)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid charge_id"})
			return
		}
		charge, err := q.GetChargeByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && charge.AdmissionID != adm.ID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "charge not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		chargeID = id
	}
	consent, err := q.InsertBillingConsent(c.Request.Context(), sqlcgen.InsertBillingConsentParams{
		CompanyID: adm.CompanyID, MerchantID: adm.MerchantID, AdmissionID: adm.ID, ChargeID: chargeID,
		ConsentedByName: req.ConsentedByName, Relation: req.Relation, RecordedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"id": consent.ID.String(),
		"charge_id": func() any {
			if !consent.ChargeID.Valid {
				return nil
			}
			return consent.ChargeID.String()
		}(),
		"kind": consent.Kind, "consented_by_name": consent.ConsentedByName,
		"relation": consent.Relation, "recorded_at": tsJSON(consent.RecordedAt),
	}, "meta": gin.H{}})
}

// FinalizeBillingHandler godoc
// @Summary Finalize the billing account (freezes the splits and totals)
// @Tags billing
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /admissions/{id}/billing/finalize [post]
func FinalizeBillingHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, _, ok := loadBillingAdmission(c, c.Param("id"), q, PermInvoiceCreate)
	if !ok {
		return
	}
	acct, err := q.GetBillingAccountByAdmission(c.Request.Context(), adm.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is not open"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	acct, err = q.GetBillingAccountForUpdate(c.Request.Context(), acct.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if acct.Status != "open" {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is not open"})
		return
	}
	n, err := q.CountActiveChargesByAdmission(c.Request.Context(), adm.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no charges"})
		return
	}
	rec, err := recomputeAccount(c, q, acct)
	if err != nil {
		if errors.Is(err, errAlreadyResponded) {
			return
		}
		if errors.Is(err, errPriceMissing) {
			c.JSON(http.StatusConflict, gin.H{"error": "price_missing"})
			return
		}
		respondInternalError(c, err)
		return
	}
	// Out-of-benefit (BPJS excluded) charges need a recorded consent.
	if len(rec.split.NeedsConsent) > 0 {
		consents, err := q.ListBillingConsentsByAdmission(c.Request.Context(), adm.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		blanket := false
		consented := map[string]bool{}
		for _, cs := range consents {
			if !cs.ChargeID.Valid {
				blanket = true
			} else {
				consented[cs.ChargeID.String()] = true
			}
		}
		missing := []string{}
		for _, id := range rec.split.NeedsConsent {
			if !blanket && !consented[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "consent_required", "charge_ids": missing})
			return
		}
	}
	// Plan-requires-GL payers covering this care type need a valid letter.
	for _, gl := range rec.gl {
		if !gl.OK {
			c.JSON(http.StatusConflict, gin.H{"error": "guarantee letter required for " + gl.Name})
			return
		}
	}
	finalized, err := q.FinalizeBillingAccount(c.Request.Context(), sqlcgen.FinalizeBillingAccountParams{
		ID: acct.ID, FinalizedBy: AuthUserID(c),
		PatientTotal: centsNum(rec.split.PatientCents), PayerTotal: centsNum(rec.split.PayerCents),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	payload, err := billingPayload(c, q, adm.ID, &finalized)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
}

type reopenBillingRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// ReopenBillingHandler godoc
// @Summary Reopen a finalized billing account (billing.account.reopen)
// @Tags billing
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body reopenBillingRequest true "Reopen reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /admissions/{id}/billing/reopen [post]
func ReopenBillingHandler(c *gin.Context) {
	var req reopenBillingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	adm, _, ok := loadBillingAdmission(c, c.Param("id"), q, PermAccountReopen)
	if !ok {
		return
	}
	acct, err := q.GetBillingAccountByAdmission(c.Request.Context(), adm.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is not finalized"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	acct, err = q.GetBillingAccountForUpdate(c.Request.Context(), acct.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if acct.Status != "finalized" {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is not finalized"})
		return
	}
	reopened, err := q.ReopenBillingAccount(c.Request.Context(), sqlcgen.ReopenBillingAccountParams{
		ID: acct.ID, LastReopenReason: optText(req.Reason), UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	payload, err := billingPayload(c, q, adm.ID, &reopened)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
}
