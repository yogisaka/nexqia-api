package server

import (
	"errors"
	"testing"
	"time"
)

// splitTestPrice builds a splitPrice from list|item → cents.
func splitTestPrice(m map[string]int64) splitPrice {
	return func(listID, itemID string, on time.Time) (int64, bool, error) {
		c, ok := m[listID+"|"+itemID]
		return c, ok, nil
	}
}

var errSplitPriceBoom = errors.New("split price boom")

func TestSplitAccount(t *testing.T) {
	day := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	glValid := func(t time.Time) bool { return true }
	glInvalid := func(t time.Time) bool { return false }

	// Each case: charges, payers, defaultListID "def", price map; check closure
	// verifies parts/totals per spec §4 (values in cents).
	cases := []struct {
		name    string
		charges []splitCharge
		payers  []splitPayer
		price   map[string]int64
		priceFn splitPrice
		check   func(t *testing.T, res splitResult, err error)
	}{
		{
			name:    "uncovered",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			price:   map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PatientCents != 100000 || res.PayerCents != 0 {
					t.Fatalf("patient=%d payer=%d, want 100000/0", res.PatientCents, res.PayerCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "patient" || parts[0].Coverage != "uncovered" || parts[0].AmountCents != 100000 {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "capitation_locked",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "capitation"},
				{PayerID: "swasta", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"bpjs-list|it": 50000, "def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PatientCents != 0 || res.PayerCents != 0 {
					t.Fatalf("patient=%d payer=%d, want 0/0", res.PatientCents, res.PayerCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Coverage != "capitation" || parts[0].AmountCents != 0 || parts[0].ShadowCents != 50000 || !parts[0].HasShadow {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "claim_anc",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list",
					Coverage: map[string]bpjsCoverage{"item:it": {Coverage: "claim", ClaimType: "anc"}}},
			},
			price: map[string]int64{"bpjs-list|it": 70000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 70000 || res.PatientCents != 0 {
					t.Fatalf("payer=%d patient=%d, want 70000/0", res.PayerCents, res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Coverage != "claim" || parts[0].ClaimType != "anc" || parts[0].AmountCents != 70000 {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "package_fkrtl",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "package"},
			},
			price: map[string]int64{"bpjs-list|it": 50000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Coverage != "package" || parts[0].AmountCents != 0 || parts[0].ShadowCents != 50000 || !parts[0].HasShadow {
					t.Fatalf("parts=%+v", parts)
				}
				if res.PatientCents != 0 || res.PayerCents != 0 {
					t.Fatalf("patient=%d payer=%d, want 0/0", res.PatientCents, res.PayerCents)
				}
			},
		},
		{
			name:    "excluded_to_patient",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "excluded"},
			},
			price: map[string]int64{"def|it": 120000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PatientCents != 120000 {
					t.Fatalf("patient=%d, want 120000", res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "patient" || parts[0].Coverage != "excluded" {
					t.Fatalf("parts=%+v", parts)
				}
				if len(res.NeedsConsent) != 1 || res.NeedsConsent[0] != "c1" {
					t.Fatalf("NeedsConsent=%v", res.NeedsConsent)
				}
			},
		},
		{
			name:    "excluded_then_private",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "excluded"},
				{PayerID: "swasta", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 120000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 120000 || res.PatientCents != 0 {
					t.Fatalf("payer=%d patient=%d, want 120000/0", res.PayerCents, res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].PayerID != "swasta" || parts[0].AmountCents != 120000 {
					t.Fatalf("parts=%+v", parts)
				}
				if len(res.NeedsConsent) != 0 {
					t.Fatalf("NeedsConsent=%v", res.NeedsConsent)
				}
			},
		},
		{
			name:    "excluded_partial_private",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "excluded"},
				{PayerID: "swasta", PayerType: "private", PriceListID: "def", AnnualRemainingCents: 62500},
			},
			price: map[string]int64{"def|it": 120000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 62500 || res.PatientCents != 57500 {
					t.Fatalf("payer=%d patient=%d, want 62500/57500", res.PayerCents, res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 2 || parts[0].PayerID != "swasta" || parts[0].AmountCents != 62500 || parts[0].Note != "annual limit" {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[1].Party != "patient" || parts[1].Coverage != "excluded" || parts[1].AmountCents != 57500 {
					t.Fatalf("parts=%+v", parts)
				}
				if len(res.NeedsConsent) != 0 {
					t.Fatalf("NeedsConsent=%v", res.NeedsConsent)
				}
			},
		},
		{
			name:    "blacklist_next_payer",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", Blacklist: map[string]bool{"it": true}},
				{PayerID: "b", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].PayerID != "b" || parts[0].AmountCents != 100000 {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "discount_percent",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, DiscountBP: map[string]int64{"it": 1000}}, // 10% = 1000 BP
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 90000 || res.PatientCents != 0 {
					t.Fatalf("payer=%d patient=%d, want 90000/0", res.PayerCents, res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].DiscountCents != 10000 || parts[0].Note != "discount" {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name: "inner_limit",
			charges: []splitCharge{
				{ID: "c1", ItemID: "obat1", ItemType: "drug", QtyMilli: 1000, ServiceDate: day},
				{ID: "c2", ItemID: "obat2", ItemType: "drug", QtyMilli: 1000, ServiceDate: day},
			},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, InnerLimitCents: map[string]int64{"drug": 50000}},
			},
			price: map[string]int64{"def|obat1": 30000, "def|obat2": 30000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 50000 || res.PatientCents != 10000 {
					t.Fatalf("payer=%d patient=%d, want 50000/10000", res.PayerCents, res.PatientCents)
				}
				p1 := res.ByCharge["c1"]
				if len(p1) != 1 || p1[0].AmountCents != 30000 {
					t.Fatalf("c1 parts=%+v", p1)
				}
				p2 := res.ByCharge["c2"]
				if len(p2) != 2 || p2[0].AmountCents != 20000 || p2[0].Note != "inner limit" || p2[1].AmountCents != 10000 {
					t.Fatalf("c2 parts=%+v", p2)
				}
			},
		},
		{
			name:    "annual_limit",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: 80000},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 80000 || res.PatientCents != 20000 {
					t.Fatalf("payer=%d patient=%d, want 80000/20000", res.PayerCents, res.PatientCents)
				}
				if res.ByCharge["c1"][0].Note != "annual limit" {
					t.Fatalf("note=%q", res.ByCharge["c1"][0].Note)
				}
			},
		},
		{
			name:    "plan_without_rule_skipped",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", HasPlan: true, AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 0 || res.PatientCents != 100000 {
					t.Fatalf("payer=%d patient=%d, want 0/100000", res.PayerCents, res.PatientCents)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "patient" || parts[0].Coverage != "" || parts[0].AmountCents != 100000 {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "gl_missing",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, RequiresGL: true, GLValidOn: glInvalid},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 2 || parts[0].PayerID != "a" || parts[0].AmountCents != 0 || parts[0].Note != "no GL" {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[1].AmountCents != 100000 || res.PatientCents != 100000 {
					t.Fatalf("patient parts=%+v total=%d", parts[1], res.PatientCents)
				}
			},
		},
		{
			name: "gl_limit",
			charges: []splitCharge{
				{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
				{ID: "c2", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
			},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, RequiresGL: true, GLCents: 150000, GLValidOn: glValid},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				if res.PayerCents != 150000 || res.PatientCents != 50000 {
					t.Fatalf("payer=%d patient=%d, want 150000/50000", res.PayerCents, res.PatientCents)
				}
				if res.ByCharge["c1"][0].AmountCents != 100000 {
					t.Fatalf("c1=%+v", res.ByCharge["c1"])
				}
				if res.ByCharge["c2"][0].AmountCents != 50000 || res.ByCharge["c2"][0].Note != "GL limit" {
					t.Fatalf("c2=%+v", res.ByCharge["c2"])
				}
			},
		},
		{
			name: "deductible_once",
			charges: []splitCharge{
				{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
				{ID: "c2", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
			},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, DeductibleCents: 25000},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// 25000 deductible taken once from row 1; row 2 fully covered.
				if res.ByCharge["c1"][0].AmountCents != 75000 || res.ByCharge["c1"][0].Note != "deductible" {
					t.Fatalf("c1=%+v", res.ByCharge["c1"])
				}
				if res.ByCharge["c2"][0].AmountCents != 100000 {
					t.Fatalf("c2=%+v", res.ByCharge["c2"])
				}
				if res.PayerCents != 175000 || res.PatientCents != 25000 {
					t.Fatalf("payer=%d patient=%d, want 175000/25000", res.PayerCents, res.PatientCents)
				}
			},
		},
		{
			name: "copay_with_cap",
			charges: []splitCharge{
				{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
				{ID: "c2", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day},
			},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, CopayBP: 1000, CopayMaxCents: 15000}, // 10% max 15000
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// Row 1: copay 10000 (10% of 100000). Row 2: copay 10000 but cap
				// leaves 15000-10000=5000 → patient 5000.
				if res.PatientCents != 15000 || res.PayerCents != 185000 {
					t.Fatalf("patient=%d payer=%d, want 15000/185000", res.PatientCents, res.PayerCents)
				}
				if res.ByCharge["c1"][1].AmountCents != 10000 || res.ByCharge["c2"][1].AmountCents != 5000 {
					t.Fatalf("c1=%+v c2=%+v", res.ByCharge["c1"], res.ByCharge["c2"])
				}
			},
		},
		{
			name:    "cob_two_private",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, VisitLimitCents: 60000},
				{PayerID: "b", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				parts := res.ByCharge["c1"]
				if len(parts) != 2 || parts[0].PayerID != "a" || parts[0].AmountCents != 60000 ||
					parts[1].PayerID != "b" || parts[1].AmountCents != 40000 {
					t.Fatalf("parts=%+v", parts)
				}
			},
		},
		{
			name:    "qty_fraction",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2500, ServiceDate: day}},
			price:   map[string]int64{"def|it": 1001},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// 1001 * 2.5 = 2502.5 → half-up = 2503.
				if res.PatientCents != 2503 || res.ByCharge["c1"][0].GrossCents != 2503 {
					t.Fatalf("patient=%d gross=%d, want 2503/2503", res.PatientCents, res.ByCharge["c1"][0].GrossCents)
				}
			},
		},
		{
			name:    "price_missing",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			price:   map[string]int64{},
			check: func(t *testing.T, res splitResult, err error) {
				if !errors.Is(err, errPriceMissing) {
					t.Fatalf("want errPriceMissing, got %v", err)
				}
			},
		},
		{
			name:    "price_error_propagates",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "bpjs", PayerType: "bpjs", PriceListID: "bpjs-list", DefaultCoverage: "excluded"},
			},
			priceFn: func(listID, itemID string, on time.Time) (int64, bool, error) {
				return 0, false, errSplitPriceBoom
			},
			check: func(t *testing.T, res splitResult, err error) {
				if !errors.Is(err, errSplitPriceBoom) {
					t.Fatalf("err = %v, want errSplitPriceBoom", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := tc.priceFn
			if fn == nil {
				fn = splitTestPrice(tc.price)
			}
			res, err := splitAccount(tc.charges, tc.payers, "def", fn)
			tc.check(t, res, err)
		})
	}
}
