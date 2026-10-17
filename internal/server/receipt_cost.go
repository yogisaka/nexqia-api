package server

import (
	"math/big"
)

// Pure receipt landed-cost functions (spec inventory §4.1).
//
// Monetary values are rupiah amounts carried as exact big.Rat; split shares
// are rounded to sen (2 decimals, half-up) and any rounding remainder goes to
// the line with the largest discounted line value.

// receiptLineInput is one receipt line expressed in packs.
type receiptLineInput struct {
	PackQty             *big.Rat // quantity received in packs
	PackContent         *big.Rat // base units per pack
	BonusPackQty        *big.Rat // bonus packs (free goods) on the line
	PackPrice           *big.Rat // price per pack
	LineDiscountPercent *big.Rat // discount percentage on the line
}

// receiptCostInput aggregates the line inputs with invoice-level figures.
type receiptCostInput struct {
	Lines           []receiptLineInput
	InvoiceDiscount *big.Rat // invoice-level discount (rupiah)
	Freight         *big.Rat // freight cost (rupiah)
	VatIncluded     bool     // prices already include VAT
	VatPercent      *big.Rat // VAT percentage (e.g. 11)
	VatCreditable   bool     // buyer is a PKP (VAT creditable)
}

// receiptLineCost is the per-line result. Monetary per-unit values are
// rendered as fixed-decimal strings matching the numeric(19,6) storage.
type receiptLineCost struct {
	BaseQty       string // total base units received (packs + bonus) × content
	UnitCost      string // landed cost per base unit, 6 decimals half-up
	HnaPpnPerPack string // HNA+PPN per pack, 2 decimals half-up
}

func ratOrZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

func pow10Rat(n int) *big.Rat {
	i := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	return new(big.Rat).SetInt(i)
}

// roundRatHalfUp rounds a non-negative rational to the given number of
// decimal places, half-up.
func roundRatHalfUp(r *big.Rat, places int) *big.Rat {
	scale := pow10Rat(places)
	scaled := new(big.Rat).Mul(r, scale)
	shifted := new(big.Rat).Add(scaled, big.NewRat(1, 2))
	q := new(big.Int).Quo(shifted.Num(), shifted.Denom()) // truncate toward zero
	return new(big.Rat).SetFrac(q, scale.Num())
}

// lineValue returns packQty × packPrice × (1 − discount/100).
func lineValue(l receiptLineInput) *big.Rat {
	qty := ratOrZero(l.PackQty)
	price := ratOrZero(l.PackPrice)
	disc := ratOrZero(l.LineDiscountPercent)
	factor := new(big.Rat).Sub(big.NewRat(100, 1), disc)
	factor.Quo(factor, big.NewRat(100, 1))
	v := new(big.Rat).Mul(qty, price)
	return v.Mul(v, factor)
}

// splitInvoiceAmount divides amount proportionally to each line's discounted
// value (each share rounded to sen, half-up); the rounding remainder in sen
// is added to the line with the largest line value.
func splitInvoiceAmount(lines []receiptLineInput, values []*big.Rat, amount *big.Rat) []*big.Rat {
	shares := make([]*big.Rat, len(lines))
	for i := range shares {
		shares[i] = new(big.Rat)
	}
	if amount == nil || amount.Sign() == 0 || len(lines) == 0 {
		return shares
	}
	total := new(big.Rat)
	for _, v := range values {
		total.Add(total, v)
	}
	if total.Sign() == 0 {
		// No proportional basis: whole amount to the first line.
		shares[0].Set(roundRatHalfUp(amount, 2))
		return shares
	}
	sum := new(big.Rat)
	for i, v := range values {
		share := new(big.Rat).Mul(amount, v)
		share.Quo(share, total)
		shares[i] = roundRatHalfUp(share, 2)
		sum.Add(sum, shares[i])
	}
	remainder := new(big.Rat).Sub(amount, sum)
	if remainder.Sign() != 0 {
		largest := 0
		for i, v := range values {
			if v.Cmp(values[largest]) > 0 {
				largest = i
			}
		}
		shares[largest].Add(shares[largest], remainder)
	}
	return shares
}

// computeReceiptLineCosts applies the contract order literally per line:
// line value → invoice discount & freight split → VAT adjustment → unit cost.
func computeReceiptLineCosts(in receiptCostInput) []receiptLineCost {
	n := len(in.Lines)
	results := make([]receiptLineCost, n)
	if n == 0 {
		return results
	}

	values := make([]*big.Rat, n)
	for i, l := range in.Lines {
		values[i] = lineValue(l)
	}

	invoiceShares := splitInvoiceAmount(in.Lines, values, ratOrZero(in.InvoiceDiscount))
	freightShares := splitInvoiceAmount(in.Lines, values, ratOrZero(in.Freight))

	vat := ratOrZero(in.VatPercent)
	vatFactor := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(vat, big.NewRat(100, 1)))

	for i, l := range in.Lines {
		value := new(big.Rat).Set(values[i])
		value.Sub(value, invoiceShares[i])
		value.Add(value, freightShares[i])

		switch {
		case !in.VatIncluded && !in.VatCreditable:
			value.Mul(value, vatFactor)
		case in.VatIncluded && in.VatCreditable:
			value.Quo(value, vatFactor)
		}

		baseQty := new(big.Rat).Add(ratOrZero(l.PackQty), ratOrZero(l.BonusPackQty))
		baseQty.Mul(baseQty, ratOrZero(l.PackContent))

		unitCost := new(big.Rat)
		if baseQty.Sign() != 0 {
			unitCost.Quo(value, baseQty)
		}

		hna := ratOrZero(l.PackPrice)
		if !in.VatIncluded {
			hna = new(big.Rat).Mul(hna, vatFactor)
		}

		results[i] = receiptLineCost{
			BaseQty:       roundRatHalfUp(baseQty, 6).FloatString(6),
			UnitCost:      roundRatHalfUp(unitCost, 6).FloatString(6),
			HnaPpnPerPack: roundRatHalfUp(hna, 2).FloatString(2),
		}
	}
	return results
}
