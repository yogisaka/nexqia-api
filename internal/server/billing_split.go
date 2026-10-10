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
// valid at time `on` (the charge's ServiceDate), already HET-capped. A
// non-nil error (anything but a missing price) is propagated by the engine.
type splitPrice func(priceListID, itemID string, on time.Time) (int64, bool, error)

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

		grossAt := func(listID string) (int64, int64, error) {
			u, ok, err := price(listID, ch.ItemID, ch.ServiceDate)
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				return 0, 0, fmt.Errorf("%w: charge %s (list %s, item %s)", errPriceMissing, ch.ID, listID, ch.ItemID)
			}
			return u, roundHalfUpDiv(u*ch.QtyMilli, 1000), nil
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
					u, g, err := grossAt(p.PriceListID)
					if err != nil {
						return splitResult{}, err
					}
					parts = append(parts, splitPart{
						Party: "payer", PayerID: p.PayerID, Coverage: cov.Coverage,
						ClaimType: cov.ClaimType, PriceListID: p.PriceListID,
						UnitCents: u, GrossCents: g, ShadowCents: g, HasShadow: true,
					})
					remaining, locked = 0, true
				case "claim":
					u, g, err := grossAt(p.PriceListID)
					if err != nil {
						return splitResult{}, err
					}
					parts = append(parts, splitPart{
						Party: "payer", PayerID: p.PayerID, Coverage: "claim",
						ClaimType: cov.ClaimType, PriceListID: p.PriceListID,
						UnitCents: u, GrossCents: g, AmountCents: g,
					})
					remaining, locked = 0, true
				default: // "excluded" (and empty): pass on at the default list
					u, g, err := grossAt(defaultListID)
					if err != nil {
						return splitResult{}, err
					}
					excluded = true
					unit, gross, remaining, list = u, g, g, defaultListID
				}
				continue
			}

			// Private payer: sequential cut from the remainder.
			if p.Blacklist[ch.ItemID] {
				continue
			}
			if !baseSet {
				u0, g0, err := grossAt(defaultListID)
				if err != nil {
					return splitResult{}, err
				}
				unit, gross, remaining, list = u0, g0, g0, defaultListID
				baseSet = true
			}
			if remaining <= 0 {
				break
			}
			u, g, err := grossAt(p.PriceListID)
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
			u, g, err := grossAt(defaultListID)
			if err != nil {
				return splitResult{}, err
			}
			unit, gross, remaining, list, baseSet = u, g, g, defaultListID, true
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
