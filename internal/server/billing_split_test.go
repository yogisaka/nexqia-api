package server

import (
	"errors"
	"testing"
	"time"
)

// splitTestPrice builds a splitPrice from list|item → cents; each unit price
// carries a single MEDIS component worth the full amount.
func splitTestPrice(m map[string]int64) splitPrice {
	return func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
		c, ok := m[listID+"|"+itemID]
		if !ok {
			return 0, nil, false, nil
		}
		return c, map[string]int64{"MEDIS": c}, true, nil
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
			name:    "components_proportional",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 1000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", AnnualRemainingCents: -1, CopayBP: 1000, CopayMaxCents: -1}, // 10% co-pay
			},
			priceFn: func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
				// Unit: MEDIS 60000 + SARANA 40000 = 100000.
				return 100000, map[string]int64{"MEDIS": 60000, "SARANA": 40000}, true, nil
			},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// Co-pay 10%: the payer bears 90000, the patient 10000.
				parts := res.ByCharge["c1"]
				if len(parts) != 2 {
					t.Fatalf("parts=%+v", parts)
				}
				payer, patient := parts[0], parts[1]
				if payer.Party != "payer" || payer.AmountCents != 90000 ||
					payer.Components["MEDIS"] != 54000 || payer.Components["SARANA"] != 36000 {
					t.Fatalf("payer part=%+v", payer)
				}
				if patient.Party != "patient" || patient.AmountCents != 10000 ||
					patient.Components["MEDIS"] != 6000 || patient.Components["SARANA"] != 4000 {
					t.Fatalf("patient part=%+v", patient)
				}
			},
		},
		{
			name:    "components_qty_scaled",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			priceFn: func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
				// Unit: MEDIS 60000 + SARANA 40000 = 100000; qty 2 → gross 200000.
				return 100000, map[string]int64{"MEDIS": 60000, "SARANA": 40000}, true, nil
			},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// lineComponents: MEDIS 60000×2=120000, SARANA 40000×2=80000,
				// remainder 200000−200000=0 → Σ components == gross (invariant).
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "patient" {
					t.Fatalf("parts=%+v", parts)
				}
				p := parts[0]
				if p.GrossCents != 200000 || p.AmountCents != 200000 {
					t.Fatalf("gross=%d amount=%d, want 200000/200000", p.GrossCents, p.AmountCents)
				}
				if p.Components["MEDIS"] != 120000 || p.Components["SARANA"] != 80000 {
					t.Fatalf("components=%+v, want MEDIS 120000 / SARANA 80000", p.Components)
				}
			},
		},
		{
			name:    "components_qty_fraction_rounding",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2500, ServiceDate: day}},
			priceFn: func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
				// Unit: MEDIS 333 + SARANA 667 = 1000; qty 2.5 → gross 2500.
				return 1000, map[string]int64{"MEDIS": 333, "SARANA": 667}, true, nil
			},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// MEDIS roundHalfUpDiv(333×2500,1000)=833, SARANA=1668;
				// remainder 2500−2501=−1 goes to the largest (SARANA) → 1667.
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].GrossCents != 2500 {
					t.Fatalf("parts=%+v", parts)
				}
				comp := parts[0].Components
				if comp["MEDIS"] != 833 || comp["SARANA"] != 1667 {
					t.Fatalf("components=%+v, want MEDIS 833 / SARANA 1667 (Σ 2500)", comp)
				}
			},
		},
		{
			name:    "contract_rate_cheaper_list",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "pl", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000, "pl|it": 80000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// G = 100000×2 = 200000; gP = 160000 < G → payer bears 160000,
				// the 40000 difference flows to the patient (pass-through).
				parts := res.ByCharge["c1"]
				if len(parts) != 2 {
					t.Fatalf("parts=%+v", parts)
				}
				payer, patient := parts[0], parts[1]
				if payer.UnitCents != 100000 || payer.GrossCents != 200000 || payer.DiscountCents != 0 ||
					payer.AmountCents != 160000 || payer.Note != "contract rate" {
					t.Fatalf("payer part=%+v", payer)
				}
				if patient.AmountCents != 40000 {
					t.Fatalf("patient part=%+v", patient)
				}
				if res.PayerCents != 160000 || res.PatientCents != 40000 {
					t.Fatalf("payer=%d patient=%d, want 160000/40000", res.PayerCents, res.PatientCents)
				}
				// Invariant I2: gross == Σ AmountCents + Σ DiscountCents.
				var sumAmt, sumDisc int64
				for _, p := range parts {
					sumAmt += p.AmountCents
					sumDisc += p.DiscountCents
				}
				if sumAmt+sumDisc != 200000 {
					t.Fatalf("invariant: %d+%d != 200000", sumAmt, sumDisc)
				}
			},
		},
		{
			name:    "contract_rate_with_rule_discount",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "pl", AnnualRemainingCents: -1,
					DiscountBP: map[string]int64{"it": 1000}}, // 10%
			},
			price: map[string]int64{"def|it": 100000, "pl|it": 80000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// cover = 160000 after cap; discount 10% of cover = 16000;
				// amount 144000; remaining = 200000−144000−16000 = 40000.
				parts := res.ByCharge["c1"]
				if len(parts) != 2 {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[0].DiscountCents != 16000 || parts[0].AmountCents != 144000 ||
					parts[0].Note != "contract rate, discount" {
					t.Fatalf("payer part=%+v", parts[0])
				}
				if parts[1].AmountCents != 40000 {
					t.Fatalf("patient part=%+v", parts[1])
				}
				// Invariant I2: 144000+40000+16000 = 200000.
				var sumAmt, sumDisc int64
				for _, p := range parts {
					sumAmt += p.AmountCents
					sumDisc += p.DiscountCents
				}
				if sumAmt+sumDisc != 200000 {
					t.Fatalf("invariant: %d+%d != 200000", sumAmt, sumDisc)
				}
			},
		},
		{
			name:    "contract_rate_higher_list_no_change",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "pl", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000, "pl|it": 125000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// gP = 250000 ≥ G 200000 → no cap: money identical to the old
				// engine; display switches to the Umum basis (100000/200000).
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "payer" {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[0].AmountCents != 200000 || parts[0].Note != "" {
					t.Fatalf("payer part=%+v", parts[0])
				}
				if parts[0].UnitCents != 100000 || parts[0].GrossCents != 200000 {
					t.Fatalf("display unit=%d gross=%d, want 100000/200000", parts[0].UnitCents, parts[0].GrossCents)
				}
			},
		},
		{
			name:    "contract_rate_equal_list_golden",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "same", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000, "same|it": 100000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// Same price → money AND display identical to the existing
				// def-list payer subtest (I3a).
				parts := res.ByCharge["c1"]
				if len(parts) != 1 || parts[0].Party != "payer" {
					t.Fatalf("parts=%+v", parts)
				}
				p := parts[0]
				if p.UnitCents != 100000 || p.GrossCents != 200000 || p.AmountCents != 200000 || p.Note != "" {
					t.Fatalf("part=%+v", p)
				}
			},
		},
		{
			name:    "contract_rate_cob_second_payer",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "def", VisitLimitCents: 60000, AnnualRemainingCents: -1},
				{PayerID: "b", PayerType: "private", PriceListID: "pl2", AnnualRemainingCents: -1},
			},
			price: map[string]int64{"def|it": 100000, "pl2|it": 60000},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// A: visit limit 60000 → cover 60000; remaining 140000.
				// B: gP = 120000 < 140000 → cover 120000; remaining 20000.
				parts := res.ByCharge["c1"]
				if len(parts) != 3 {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[0].PayerID != "a" || parts[0].AmountCents != 60000 || parts[0].Note != "visit limit" {
					t.Fatalf("A part=%+v", parts[0])
				}
				if parts[1].PayerID != "b" || parts[1].AmountCents != 120000 || parts[1].Note != "contract rate" {
					t.Fatalf("B part=%+v", parts[1])
				}
				if parts[2].AmountCents != 20000 {
					t.Fatalf("patient part=%+v", parts[2])
				}
				// Invariant I2: 60000+120000+20000 = 200000.
				var sumAmt, sumDisc int64
				for _, p := range parts {
					sumAmt += p.AmountCents
					sumDisc += p.DiscountCents
				}
				if sumAmt+sumDisc != 200000 {
					t.Fatalf("invariant: %d+%d != 200000", sumAmt, sumDisc)
				}
			},
		},
		{
			name:    "contract_rate_components_capped",
			charges: []splitCharge{{ID: "c1", ItemID: "it", ItemType: "visit", QtyMilli: 2000, ServiceDate: day}},
			payers: []splitPayer{
				{PayerID: "a", PayerType: "private", PriceListID: "pl", AnnualRemainingCents: -1},
			},
			priceFn: func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
				switch listID {
				case "def":
					// Unit: MEDIS 60000 + SARANA 40000 = 100000; qty 2 → gross 200000.
					return 100000, map[string]int64{"MEDIS": 60000, "SARANA": 40000}, true, nil
				default: // "pl": contract rate 80000
					return 80000, nil, true, nil
				}
			},
			check: func(t *testing.T, res splitResult, err error) {
				if err != nil {
					t.Fatal(err)
				}
				// Line components (Umum basis): MEDIS 120000, SARANA 80000.
				// Payer cover 160000 → componentShare × 160000/200000:
				// MEDIS 96000, SARANA 64000 (Σ = AmountCents).
				parts := res.ByCharge["c1"]
				if len(parts) != 2 {
					t.Fatalf("parts=%+v", parts)
				}
				line := lineComponents(map[string]int64{"MEDIS": 60000, "SARANA": 40000}, 200000, 2000)
				if line["MEDIS"] != 120000 || line["SARANA"] != 80000 {
					t.Fatalf("line components=%+v", line)
				}
				comp := parts[0].Components
				if comp["MEDIS"] != 96000 || comp["SARANA"] != 64000 {
					t.Fatalf("payer components=%+v, want MEDIS 96000 / SARANA 64000", comp)
				}
				if comp["MEDIS"]+comp["SARANA"] != parts[0].AmountCents {
					t.Fatalf("Σ components %d != amount %d", comp["MEDIS"]+comp["SARANA"], parts[0].AmountCents)
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
			priceFn: func(listID, itemID string, on time.Time) (int64, map[string]int64, bool, error) {
				return 0, nil, false, errSplitPriceBoom
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
