package server

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestDrugSellPrice covers the pure sell-price formula (spec §4.2.2):
// pack 10000000 cents ÷ factor 100 × 1.20 = 120000; ceil(416666.67) = 416667;
// ceil to 10000-multiples = 420000; HET 400000 caps 420000 → 400000;
// no margin 500000/2.5 = 200000; −10% → 90000; price at HET is not capped.
func TestDrugSellPrice(t *testing.T) {
	for _, tc := range []struct {
		pack, factor, margin, rounding, het, want int64
		capped                                    bool
	}{
		{10000000, 100000, 2000, 0, 0, 120000, false},
		{10000000, 30000, 2500, 0, 0, 416667, false},
		{10000000, 30000, 2500, 10000, 0, 420000, false},
		{10000000, 30000, 2500, 10000, 400000, 400000, true},
		{500000, 2500, 0, 0, 0, 200000, false},
		{100000, 1000, -1000, 0, 0, 90000, false},
		{100000, 1000, 0, 0, 100000, 100000, false},
	} {
		got, capped := drugSellPriceCents(tc.pack, tc.factor, tc.margin, tc.rounding, tc.het)
		if got != tc.want || capped != tc.capped {
			t.Errorf("drugSellPriceCents(%d,%d,%d,%d,%d) = %d,%v; want %d,%v",
				tc.pack, tc.factor, tc.margin, tc.rounding, tc.het, got, capped, tc.want, tc.capped)
		}
	}
}

// TestNumericToMilli: 100 → 100000, 2.5 → 2500, 0.125 → 125; 1.2345 has 4
// fraction digits → error.
func TestNumericToMilli(t *testing.T) {
	for _, tc := range []struct {
		n    pgtype.Numeric
		want int64
	}{
		{pgtype.Numeric{Int: big.NewInt(100), Exp: 0, Valid: true}, 100000},
		{pgtype.Numeric{Int: big.NewInt(2500), Exp: -3, Valid: true}, 2500},
		{pgtype.Numeric{Int: big.NewInt(125), Exp: -3, Valid: true}, 125},
	} {
		got, err := numericToMilli(tc.n)
		if err != nil || got != tc.want {
			t.Errorf("numericToMilli(%v) = %d, %v; want %d", tc.n, got, err, tc.want)
		}
	}
	if _, err := numericToMilli(pgtype.Numeric{Int: big.NewInt(12345), Exp: -4, Valid: true}); err == nil {
		t.Errorf("numericToMilli(1.2345) accepted, want error")
	}
}
