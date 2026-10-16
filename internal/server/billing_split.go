// internal/server/billing_split.go
// Pure split engine (spec 2026-10-07-tariff-b1-billing-core-design §4).
// No DB: charges, payers and a price lookup function go in; per-charge
// split parts and totals come out. Money in cents (int64), percent in
// basis points.
package server

import (
	"fmt"
	"sort"
	"time"
)

type splitCharge struct {
	ID, ItemID, ItemType string
	QtyMilli             int64
	ServiceDate          time.Time
}

// splitPrice returns the unit price (cents) on the given price list that is
// valid at time `on` (the charge's ServiceDate), already HET-capped, plus the
// per-component breakdown of that unit price (component code → cents; the
// components sum to the unit price). A non-nil error (anything but a missing
// price) is propagated by the engine.
type splitPrice func(priceListID, itemID string, on time.Time) (unitCents int64, components map[string]int64, ok bool, err error)

type splitPayer struct {
	PayerID, PayerType, PriceListID string // PriceListID resolved (payer list or merchant default)
	Coverage                        map[string]bpjsCoverage
	DefaultCoverage                 string
	Blacklist                       map[string]bool
	DiscountBP                      map[string]int64
	DiscountCents                   map[string]int64
	HasPlan                         bool  // an active, payer-matched plan is attached
	HasPlanRule                     bool  // ...and it has a rule for the visit's care type
	CopayBP, CopayMaxCents          int64 // CopayMaxCents: -1 = none
	DeductibleCents                 int64
	VisitLimitCents                 int64 // -1 / 0 = none
	InnerLimitCents                 map[string]int64
	AnnualRemainingCents            int64 // -1 = unlimited
	RequiresGL                      bool
	GLCents                         int64
	GLValidOn                       func(time.Time) bool
}

type bpjsCoverage struct {
	Coverage, ClaimType string
}

type splitPart struct {
	Party, PayerID, Coverage, ClaimType, PriceListID string
	UnitCents, GrossCents, DiscountCents             int64
	AmountCents, ShadowCents                         int64
	HasShadow                                        bool
	Note                                             string
	// Components is this party's portion of the line per price component
	// (code → cents), scaled proportionally from the unit components by
	// AmountCents/GrossCents with the rounding remainder on the largest
	// component (the pattern of resolvePrice).
	Components map[string]int64
}

// componentShare scales the unit components of a line to a party's amount
// (cents): each component half-up proportional to amount/gross, and the
// rounding remainder (either sign) goes to the largest component (ties: the
// smallest code), matching resolvePrice. Zero/negative amounts share nothing.
func componentShare(unit map[string]int64, gross, amount int64) map[string]int64 {
	if len(unit) == 0 || gross <= 0 || amount <= 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(unit))
	var sum, largestCents int64
	largest := ""
	for code, cents := range unit {
		v := roundHalfUpDiv(cents*amount, gross)
		out[code] = v
		sum += v
		if v > largestCents || (v == largestCents && (largest == "" || code < largest)) {
			largest, largestCents = code, v
		}
	}
	out[largest] += amount - sum
	return out
}

// lineComponents scales the unit components of a line to its quantity
// (milli-units): each component half-up per-unit × qty, and only the
// rounding remainder (gross − sum) goes to the largest component
// (ties: the smallest code) — the pattern of componentShare/resolvePrice.
func lineComponents(unit map[string]int64, gross, qtyMilli int64) map[string]int64 {
	if len(unit) == 0 || gross <= 0 || qtyMilli <= 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(unit))
	var sum, largestCents int64
	largest := ""
	for code, cents := range unit {
		v := roundHalfUpDiv(cents*qtyMilli, 1000)
		out[code] = v
		sum += v
		if v > largestCents || (v == largestCents && code < largest) {
			largest, largestCents = code, v
		}
	}
	if largest != "" {
		out[largest] += gross - sum
	}
	return out
}

type splitResult struct {
	ByCharge     map[string][]splitPart
	PatientCents int64
	PayerCents   int64
	NeedsConsent []string
}

// payerState accumulates per-visit/per-account usage for one payer while
// splitting successive charges.
type payerState struct {
	innerUsed      map[string]int64
	visitUsed      int64
	annualUsed     int64
	glUsed         int64
	copayUsed      int64
	deductibleLeft int64
}

func splitAccount(charges []splitCharge, payers []splitPayer, defaultListID string, price splitPrice) (splitResult, error) {
	res := splitResult{ByCharge: make(map[string][]splitPart, len(charges))}

	// BPJS payers are always processed first (K8); other payers keep input order.
	order := make([]int, len(payers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return payers[order[a]].PayerType == "bpjs" && payers[order[b]].PayerType != "bpjs"
	})

	states := make(map[string]*payerState, len(payers))
	for _, p := range payers {
		st := &payerState{innerUsed: make(map[string]int64)}
		if p.DeductibleCents > 0 {
			st.deductibleLeft = p.DeductibleCents
		}
		states[p.PayerID] = st
	}

	for _, ch := range charges {
		parts := []splitPart{}
		remaining := int64(0)
		locked := false   // BPJS fully covers (capitation/claim/package): stop here
		excluded := false // remainder originates from BPJS exclusion
		carried := false  // a private payer covered part of the excluded remainder
		unit, gross, list := int64(0), int64(0), defaultListID
		comps := map[string]int64{} // line components (per-unit × qty) of the current (list, item)

		grossAt := func(listID string) (int64, map[string]int64, int64, error) {
			u, uc, ok, err := price(listID, ch.ItemID, ch.ServiceDate)
			if err != nil {
				return 0, nil, 0, err
			}
			if !ok {
				return 0, nil, 0, fmt.Errorf("%w: charge %s (list %s, item %s)", errPriceMissing, ch.ID, listID, ch.ItemID)
			}
			g := roundHalfUpDiv(u*ch.QtyMilli, 1000)
			return u, lineComponents(uc, g, ch.QtyMilli), g, nil
		}

		baseSet := false // remainder valued at the default list, set lazily
		for _, idx := range order {
			if locked {
				break
			}
			p := payers[idx]
			st := states[p.PayerID]

			if p.PayerType == "bpjs" {
				// Coverage: item rule > item_type rule > facility default.
				covKey := "item:" + ch.ItemID
				cov, okCov := p.Coverage[covKey]
				if !okCov {
					cov, okCov = p.Coverage["type:"+ch.ItemType]
				}
				if !okCov || cov.Coverage == "" {
					cov.Coverage = p.DefaultCoverage
				}
				switch cov.Coverage {
				case "capitation", "package":
					u, uc, g, err := grossAt(p.PriceListID)
					if err != nil {
						return splitResult{}, err
					}
					parts = append(parts, splitPart{
						Party: "payer", PayerID: p.PayerID, Coverage: cov.Coverage,
						ClaimType: cov.ClaimType, PriceListID: p.PriceListID,
						UnitCents: u, GrossCents: g, ShadowCents: g, HasShadow: true,
						Components: componentShare(uc, g, 0),
					})
					remaining, locked = 0, true
				case "claim":
					u, uc, g, err := grossAt(p.PriceListID)
					if err != nil {
						return splitResult{}, err
					}
					parts = append(parts, splitPart{
						Party: "payer", PayerID: p.PayerID, Coverage: "claim",
						ClaimType: cov.ClaimType, PriceListID: p.PriceListID,
						UnitCents: u, GrossCents: g, AmountCents: g,
						Components: componentShare(uc, g, g),
					})
					remaining, locked = 0, true
				default: // "excluded" (and empty): pass on at the default list
					u, uc, g, err := grossAt(defaultListID)
					if err != nil {
						return splitResult{}, err
					}
					excluded = true
					unit, gross, remaining, list, comps = u, g, g, defaultListID, uc
				}
				continue
			}

			// Private payer: sequential cut from the remainder.
			if p.HasPlan && !p.HasPlanRule {
				continue // plan without a rule for this care type bears nothing (spec §3.4)
			}
			if p.Blacklist[ch.ItemID] {
				continue
			}
			if !baseSet {
				u0, uc0, g0, err := grossAt(defaultListID)
				if err != nil {
					return splitResult{}, err
				}
				unit, gross, remaining, list, comps = u0, g0, g0, defaultListID, uc0
				baseSet = true
			}
			if remaining <= 0 {
				break
			}
			u, uc, g, err := grossAt(p.PriceListID)
			if err != nil {
				return splitResult{}, err
			}
			cover := remaining
			var notes []string

			discount := int64(0)
			if bp := p.DiscountBP[ch.ItemID]; bp > 0 {
				discount += roundHalfUpDiv(cover*bp, 10000)
			}
			if perUnit := p.DiscountCents[ch.ItemID]; perUnit > 0 {
				discount += roundHalfUpDiv(perUnit*ch.QtyMilli, 1000)
			}
			if discount > cover {
				discount = cover
			}
			if discount > 0 {
				cover -= discount
				notes = append(notes, "discount")
			}

			if limit := p.InnerLimitCents[ch.ItemType]; limit > 0 {
				avail := limit - st.innerUsed[ch.ItemType]
				if avail < 0 {
					avail = 0
				}
				if cover > avail {
					cover = avail
					notes = append(notes, "inner limit")
				}
				st.innerUsed[ch.ItemType] += cover
			}

			if p.VisitLimitCents > 0 {
				avail := p.VisitLimitCents - st.visitUsed
				if avail < 0 {
					avail = 0
				}
				if cover > avail {
					cover = avail
					notes = append(notes, "visit limit")
				}
				st.visitUsed += cover
			}

			if p.AnnualRemainingCents >= 0 {
				avail := p.AnnualRemainingCents - st.annualUsed
				if avail < 0 {
					avail = 0
				}
				if cover > avail {
					cover = avail
					notes = append(notes, "annual limit")
				}
				st.annualUsed += cover
			}

			if p.RequiresGL {
				if p.GLValidOn == nil || !p.GLValidOn(ch.ServiceDate) {
					cover = 0
					notes = append(notes, "no GL")
				} else if p.GLCents > 0 {
					avail := p.GLCents - st.glUsed
					if avail < 0 {
						avail = 0
					}
					if cover > avail {
						cover = avail
						notes = append(notes, "GL limit")
					}
					st.glUsed += cover
				}
			}

			if p.DeductibleCents > 0 && st.deductibleLeft > 0 {
				d := cover
				if d > st.deductibleLeft {
					d = st.deductibleLeft
				}
				cover -= d
				st.deductibleLeft -= d
				notes = append(notes, "deductible")
			}

			if p.CopayBP > 0 {
				share := roundHalfUpDiv(cover*p.CopayBP, 10000)
				if p.CopayMaxCents >= 0 {
					avail := p.CopayMaxCents - st.copayUsed
					if avail < 0 {
						avail = 0
					}
					if share > avail {
						share = avail
					}
					st.copayUsed += share
				}
				if share > 0 {
					cover -= share
					notes = append(notes, "copay")
				}
			}

			if cover > 0 || len(notes) > 0 {
				parts = append(parts, splitPart{
					Party: "payer", PayerID: p.PayerID, PriceListID: p.PriceListID,
					UnitCents: u, GrossCents: g, DiscountCents: discount,
					AmountCents: cover, Note: joinNotes(notes),
					Components: componentShare(uc, g, cover),
				})
				// Contractual discount is waived (absorbed by the facility),
				// not passed on to the next payer or the patient.
				remaining -= cover + discount
				if cover > 0 {
					carried = true
				}
			}
		}

		if !baseSet && !locked { // no payer touched the remainder: patient pays in full
			u, uc, g, err := grossAt(defaultListID)
			if err != nil {
				return splitResult{}, err
			}
			unit, gross, remaining, list, comps, baseSet = u, g, g, defaultListID, uc, true
		}
		if remaining > 0 {
			cov := ""
			switch {
			case len(payers) == 0:
				cov = "uncovered"
			case excluded:
				cov = "excluded"
				if !carried {
					res.NeedsConsent = append(res.NeedsConsent, ch.ID)
				}
			}
			parts = append(parts, splitPart{
				Party: "patient", Coverage: cov, PriceListID: list,
				UnitCents: unit, GrossCents: gross, AmountCents: remaining,
				Components: componentShare(comps, gross, remaining),
			})
		}

		res.ByCharge[ch.ID] = parts
		for _, pt := range parts {
			if pt.Party == "patient" {
				res.PatientCents += pt.AmountCents
			} else {
				res.PayerCents += pt.AmountCents
			}
		}
	}
	return res, nil
}

func joinNotes(notes []string) string {
	out := ""
	for i, n := range notes {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
