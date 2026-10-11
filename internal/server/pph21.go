package server

// Pure PPh 21 computation helpers per spec 2026-10-07-tariff-d-practitioner-fees-tax
// §4.3.4. All money values are cents (int64); rates are basis points
// (100 = 1%). Reference values (TER, PTKP, Pasal 17, biaya jabatan,
// partner base) are loaded from the core.tax_* tables by Task 5's
// loadTaxTables; -1 as UpperInclusive means "no upper bound".

// terRow is one monthly TER (PP 58/2023) rate row, values in cents.
type terRow struct {
	Category       string // "A" | "B" | "C"
	LowerExclusive int64
	UpperInclusive int64 // -1 = none
	RateBP         int64
}

// taxBracket is one progressive Pasal 17 bracket, values in cents.
type taxBracket struct {
	UpperInclusive int64 // -1 = none
	RateBP         int64
}

// pph21Tables bundles the reference data needed by the pure PPh 21 functions.
type pph21Tables struct {
	TER                   []terRow
	PTKP                  map[string]int64 // status → annual PTKP, cents
	Pasal17               []taxBracket
	BiayaJabatanBP        int64 // percent of annual gross, basis points
	BiayaJabatanMaxAnnual int64 // cap, cents/year
	PartnerBaseBP         int64 // percent of gross forming the partner tax base
}

// terCategory maps a PTKP status to its TER category:
// A: TK/0, TK/1, K/0; B: TK/2, TK/3, K/1, K/2; C: K/3.
func terCategory(ptkp string) string {
	switch ptkp {
	case "TK/0", "TK/1", "K/0":
		return "A"
	case "TK/2", "TK/3", "K/1", "K/2":
		return "B"
	case "K/3":
		return "C"
	}
	return ""
}

// pasal17 computes progressive Pasal 17 tax on an annual PKP (cents) and
// floors the result to a whole rupiah (multiple of 100 cents).
func pasal17(pkpCents int64, br []taxBracket) int64 {
	var scaled int64
	prev := int64(0)
	for _, b := range br {
		upper := b.UpperInclusive
		if upper < 0 || upper > pkpCents {
			upper = pkpCents
		}
		if upper > prev {
			scaled += (upper - prev) * b.RateBP
			prev = upper
		}
		if prev >= pkpCents {
			break
		}
	}
	return floorRupiahCents(scaled / 10000)
}

// pph21Partner computes the partner_50pct tax: base = PartnerBaseBP × gross,
// tax = progressive Pasal 17 on that base (not cumulative across batches).
func pph21Partner(grossCents int64, t pph21Tables) (base, tax int64) {
	base = halfUpCents(grossCents*t.PartnerBaseBP, 10000)
	tax = pasal17(base, t.Pasal17)
	return base, tax
}

// pph21TERMonthly computes the ter_monthly tax for an employee month
// (January–November): the TER row where lower_exclusive < gross ≤
// upper_inclusive for the category derived from the PTKP status; tax =
// gross × rate, floored to a whole rupiah. Unknown PTKP yields rate 0.
func pph21TERMonthly(grossCents int64, ptkp string, t pph21Tables) (category string, rateBP, tax int64) {
	category = terCategory(ptkp)
	for _, row := range t.TER {
		if row.Category != category {
			continue
		}
		if grossCents <= row.LowerExclusive {
			continue
		}
		if row.UpperInclusive >= 0 && grossCents > row.UpperInclusive {
			continue
		}
		rateBP = row.RateBP
		tax = floorRupiahCents(grossCents * rateBP / 10000)
		return category, rateBP, tax
	}
	return category, 0, 0
}

// pph21December computes the annual_december tax for an employee December
// batch: biaya jabatan = min(BiayaJabatanBP × annual gross, cap); PKP =
// annual gross − biaya jabatan − PTKP (floored at 0, rounded down to a
// thousand rupiah); annual tax = progressive Pasal 17; December tax =
// annual tax − January–November tax (may be negative when over-withheld).
func pph21December(annualGrossCents int64, ptkp string, janNovTaxCents int64, t pph21Tables) (pkp, annualTax, decTax int64) {
	ptkpCents := t.PTKP[ptkp]
	biaya := halfUpCents(annualGrossCents*t.BiayaJabatanBP, 10000)
	if biaya > t.BiayaJabatanMaxAnnual {
		biaya = t.BiayaJabatanMaxAnnual
	}
	pkp = annualGrossCents - biaya - ptkpCents
	if pkp < 0 {
		pkp = 0
	}
	pkp = (pkp / 100000) * 100000 // floor to a thousand rupiah
	annualTax = pasal17(pkp, t.Pasal17)
	decTax = annualTax - janNovTaxCents
	return pkp, annualTax, decTax
}

// floorRupiahCents floors a cent amount to a whole rupiah (multiple of 100).
func floorRupiahCents(cents int64) int64 {
	return (cents / 100) * 100
}
