package server

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{"75000": 7500000, "75000.5": 7500050, "0.05": 5, "0.80": 80, "075000": 7500000, "75000.50": 7500050, " 12 ": 1200} {
		got, err := parseCents(in)
		if err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.234", "1.", ".5", "1e3", "abc", "+5", "12345678901234"} {
		if _, err := parseCents(bad); err == nil {
			t.Errorf("parseCents(%q) accepted", bad)
		}
	}
	if s := formatCents(7500050); s != "75000.50" {
		t.Errorf("formatCents = %s", s)
	}
	for _, tc := range []struct {
		n    pgtype.Numeric
		want int64
	}{
		{pgtype.Numeric{Int: big.NewInt(7500000), Exp: -2, Valid: true}, 7500000},
		{pgtype.Numeric{Int: big.NewInt(75000), Exp: 0, Valid: true}, 7500000},
		{pgtype.Numeric{Int: big.NewInt(7500000), Exp: -4, Valid: true}, 75000},
	} {
		got, err := numericToCents(tc.n)
		if err != nil || got != tc.want {
			t.Errorf("numericToCents(%v) = %d, %v; want %d", tc.n, got, err, tc.want)
		}
	}
	if _, err := numericToCents(pgtype.Numeric{Int: big.NewInt(12345), Exp: -3, Valid: true}); err == nil {
		t.Error("numericToCents must reject a non-zero third fraction digit")
	}
}

func TestResolvePrice(t *testing.T) {
	base := []rateRow{{Code: "MEDIS", Cents: 4000000}, {Code: "SARANA", Cents: 6000000}} // 40.000 + 60.000
	derived := priceListRule{Derived: true, AdjustmentBP: 3000, CategoryAdjustmentBP: map[string]int64{"drug": 0}}

	got, err := resolvePrice("procedure", derived, nil, base)
	if err != nil || got.TotalCents != 13000000 || got.Source != "derived" || got.AppliedBP != 3000 {
		t.Fatalf("+30%%: %+v, %v", got, err)
	}
	if got.Components[0].Code != "MEDIS" || got.Components[0].Cents != 5200000 || got.Components[1].Cents != 7800000 {
		t.Fatalf("+30%% components proportional: %+v", got.Components)
	}

	got, _ = resolvePrice("drug", derived, nil, base)
	if got.TotalCents != 10000000 || got.AppliedBP != 0 {
		t.Fatalf("category override 0%%: %+v", got)
	}

	own := []rateRow{{Code: "SARANA", Cents: 15000000}}
	got, _ = resolvePrice("procedure", derived, own, base)
	if got.Source != "manual" || got.TotalCents != 15000000 {
		t.Fatalf("manual wins: %+v", got)
	}

	// 100.000 + 12.5% = 112.500 → rounded to 1.000 = 113.000; +500 on the largest (SARANA 67.500 → 68.000).
	rounding := priceListRule{Derived: true, AdjustmentBP: 1250, RoundingCents: 100000}
	got, err = resolvePrice("procedure", rounding, nil, base)
	if err != nil || got.TotalCents != 11300000 {
		t.Fatalf("rounding total: %+v, %v", got, err)
	}
	if got.Components[1].Code != "SARANA" || got.Components[1].Cents != 6800000 || got.Components[0].Cents != 4500000 {
		t.Fatalf("rounding diff on largest component: %+v", got.Components)
	}

	// Tie on the largest component → the smallest code takes the difference.
	tie := []rateRow{{Code: "B", Cents: 5025}, {Code: "A", Cents: 5025}} // 100.50 → round to 1.00 → 101.00
	got, _ = resolvePrice("procedure", priceListRule{Derived: true, RoundingCents: 100}, nil, tie)
	if got.TotalCents != 10100 || got.Components[0].Code != "A" || got.Components[0].Cents != 5075 {
		t.Fatalf("tie: %+v", got.Components)
	}

	// Rounding down below zero on a tiny component set.
	tiny := []rateRow{{Code: "A", Cents: 10}}
	if _, err := resolvePrice("procedure", priceListRule{Derived: true, AdjustmentBP: -9000, RoundingCents: 1000}, nil, tiny); err != nil {
		t.Fatalf("1 cent rounds to 0, not negative: %v", err)
	}

	if _, err := resolvePrice("procedure", priceListRule{}, nil, base); err != errPriceMissing {
		t.Fatalf("standalone without own rows: want errPriceMissing, got %v", err)
	}
	if _, err := resolvePrice("procedure", derived, nil, nil); err != errPriceMissing {
		t.Fatalf("derived without base rows: want errPriceMissing, got %v", err)
	}
}
