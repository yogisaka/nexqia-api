package server

import (
	"math/big"
	"testing"
)

// ratR is a helper building a big.Rat from an integer numerator.
func ratR(n int64) *big.Rat { return big.NewRat(n, 1) }

// Expected values are computed from the spec §4.1 contract order:
// line value = packQty × packPrice × (1 − disc/100); invoice discount and
// freight split proportionally (sen rounding, remainder to largest line);
// VAT adjustments; unitCost = final value ÷ ((packQty + bonus) × content).
func TestReceiptCost(t *testing.T) {
	cases := []struct {
		name string
		in   receiptCostInput
		want []receiptLineCost
	}{
		{
			name: "simple_included",
			// 10 box @ 100.000 isi 100 → nilai 1.000.000; PPN termasuk,
			// bukan PKP → nilai tetap. unitCost = 1.000.000 / (10×100) = 1.000.
			in: receiptCostInput{
				Lines: []receiptLineInput{{
					PackQty: ratR(10), PackContent: ratR(100),
					PackPrice: ratR(100000), LineDiscountPercent: ratR(0),
				}},
				VatIncluded: true,
				VatPercent:  ratR(11),
			},
			want: []receiptLineCost{
				{BaseQty: "1000.000000", UnitCost: "1000.000000", HnaPpnPerPack: "100000.00"},
			},
		},
		{
			name: "bonus",
			// Sama dengan simple_included +1 box bonus:
			// unitCost = 1.000.000 / (11×100) = 909,090909…
			in: receiptCostInput{
				Lines: []receiptLineInput{{
					PackQty: ratR(10), PackContent: ratR(100), BonusPackQty: ratR(1),
					PackPrice: ratR(100000), LineDiscountPercent: ratR(0),
				}},
				VatIncluded: true,
				VatPercent:  ratR(11),
			},
			want: []receiptLineCost{
				{BaseQty: "1100.000000", UnitCost: "909.090909", HnaPpnPerPack: "100000.00"},
			},
		},
		{
			name: "line_and_invoice_discount",
			// 2 baris nilai 600.000 & 400.000, diskon faktur 100.000 →
			// 60.000 & 40.000 → nilai akhir 540.000 & 360.000.
			in: receiptCostInput{
				Lines: []receiptLineInput{
					{PackQty: ratR(6), PackContent: ratR(100), PackPrice: ratR(100000), LineDiscountPercent: ratR(0)},
					{PackQty: ratR(4), PackContent: ratR(100), PackPrice: ratR(100000), LineDiscountPercent: ratR(0)},
				},
				InvoiceDiscount: ratR(100000),
				VatIncluded:     true,
				VatPercent:      ratR(11),
			},
			want: []receiptLineCost{
				{BaseQty: "600.000000", UnitCost: "900.000000", HnaPpnPerPack: "100000.00"},
				{BaseQty: "400.000000", UnitCost: "900.000000", HnaPpnPerPack: "100000.00"},
			},
		},
		{
			name: "freight_split",
			// 2 baris nilai 600.000 & 400.000, ongkir 50.000 →
			// 30.000 & 20.000 → nilai akhir 630.000 & 420.000.
			in: receiptCostInput{
				Lines: []receiptLineInput{
					{PackQty: ratR(6), PackContent: ratR(100), PackPrice: ratR(100000), LineDiscountPercent: ratR(0)},
					{PackQty: ratR(4), PackContent: ratR(100), PackPrice: ratR(100000), LineDiscountPercent: ratR(0)},
				},
				Freight:     ratR(50000),
				VatIncluded: true,
				VatPercent:  ratR(11),
			},
			want: []receiptLineCost{
				{BaseQty: "600.000000", UnitCost: "1050.000000", HnaPpnPerPack: "100000.00"},
				{BaseQty: "400.000000", UnitCost: "1050.000000", HnaPpnPerPack: "100000.00"},
			},
		},
		{
			name: "vat_excluded_not_creditable",
			// Nilai 100.000 + 11% → biaya 111.000 (bukan PKP: PPN jadi biaya);
			// HNA+PPN = 100.000 × 1,11 = 111.000.
			// 1 box isi 100 → baseQty 100 → unitCost = 111.000 / 100 = 1.110.
			in: receiptCostInput{
				Lines: []receiptLineInput{{
					PackQty: ratR(1), PackContent: ratR(100),
					PackPrice: ratR(100000), LineDiscountPercent: ratR(0),
				}},
				VatIncluded:   false,
				VatPercent:    ratR(11),
				VatCreditable: false,
			},
			want: []receiptLineCost{
				{BaseQty: "100.000000", UnitCost: "1110.000000", HnaPpnPerPack: "111000.00"},
			},
		},
		{
			name: "vat_included_creditable",
			// Nilai 100.000 ÷ 1,11 = 90.090,0900900…; 1 box isi 100 →
			// unitCost = 90.090,0900… / 100 = 900,900901 (half-up 6 desimal).
			in: receiptCostInput{
				Lines: []receiptLineInput{{
					PackQty: ratR(1), PackContent: ratR(100),
					PackPrice: ratR(100000), LineDiscountPercent: ratR(0),
				}},
				VatIncluded:   true,
				VatPercent:    ratR(11),
				VatCreditable: true,
			},
			want: []receiptLineCost{
				{BaseQty: "100.000000", UnitCost: "900.900901", HnaPpnPerPack: "100000.00"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeReceiptLineCosts(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d", len(got), len(tc.want))
			}
			for i, g := range got {
				w := tc.want[i]
				if g.BaseQty != w.BaseQty {
					t.Errorf("line %d BaseQty = %s, want %s", i, g.BaseQty, w.BaseQty)
				}
				if g.UnitCost != w.UnitCost {
					t.Errorf("line %d UnitCost = %s, want %s", i, g.UnitCost, w.UnitCost)
				}
				if g.HnaPpnPerPack != w.HnaPpnPerPack {
					t.Errorf("line %d HnaPpnPerPack = %s, want %s", i, g.HnaPpnPerPack, w.HnaPpnPerPack)
				}
			}
		})
	}
}
