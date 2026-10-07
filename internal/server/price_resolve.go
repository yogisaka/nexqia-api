// internal/server/price_resolve.go
// The single price resolver (spec 2026-10-07-tariff-price-lists §4).
package server

import (
	"errors"
	"sort"
)

var (
	errPriceMissing              = errors.New("price_missing")
	errRoundingNegativeComponent = errors.New("rounding_negative_component")
)

// rateRow is one component price of an item, in cents.
type rateRow struct {
	ComponentID string
	Code        string
	Name        string
	Cents       int64
}

// priceListRule is what resolution needs from a price list: percents in
// basis points (12.50% = 1250), rounding unit in cents (0 = none).
type priceListRule struct {
	Derived              bool
	AdjustmentBP         int64
	CategoryAdjustmentBP map[string]int64
	RoundingCents        int64
}

type resolvedPrice struct {
	TotalCents int64
	Components []rateRow
	Source     string // "manual" | "derived"
	AppliedBP  int64
}

// roundHalfUpDiv returns num/den rounded half up (num >= 0, den > 0).
func roundHalfUpDiv(num, den int64) int64 {
	q, r := num/den, num%den
	if r*2 >= den {
		q++
	}
	return q
}

// resolvePrice: own rows win (manual); otherwise base rows adjusted by the
// category percent (else the list percent), rounded per component to the
// cent, then the total rounded to the list unit with the difference put on
// the largest component (ties: smallest code). No rows → errPriceMissing.
func resolvePrice(itemType string, rule priceListRule, own, base []rateRow) (resolvedPrice, error) {
	if len(own) > 0 {
		comps := append([]rateRow(nil), own...)
		sort.Slice(comps, func(i, j int) bool { return comps[i].Code < comps[j].Code })
		var total int64
		for _, c := range comps {
			total += c.Cents
		}
		return resolvedPrice{TotalCents: total, Components: comps, Source: "manual"}, nil
	}
	if !rule.Derived || len(base) == 0 {
		return resolvedPrice{}, errPriceMissing
	}
	bp := rule.AdjustmentBP
	if v, ok := rule.CategoryAdjustmentBP[itemType]; ok {
		bp = v
	}
	comps := make([]rateRow, 0, len(base))
	var total int64
	for _, b := range base {
		c := b
		c.Cents = roundHalfUpDiv(b.Cents*(10000+bp), 10000)
		comps = append(comps, c)
		total += c.Cents
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Code < comps[j].Code })
	if rule.RoundingCents > 0 {
		rounded := roundHalfUpDiv(total, rule.RoundingCents) * rule.RoundingCents
		if diff := rounded - total; diff != 0 {
			largest := 0
			for i := range comps {
				if comps[i].Cents > comps[largest].Cents {
					largest = i
				}
			}
			comps[largest].Cents += diff
			if comps[largest].Cents < 0 {
				return resolvedPrice{}, errRoundingNegativeComponent
			}
			total = rounded
		}
	}
	return resolvedPrice{TotalCents: total, Components: comps, Source: "derived", AppliedBP: bp}, nil
}
