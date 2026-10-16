package server

// Unit tests for pure fee rule selection (spec §4.1) and fee computation
// (spec §4.2). No DB involved.

import (
	"testing"
	"time"
)

var feeTestDay = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

func mkRule(id string, mod func(*feeRule)) feeRule {
	r := feeRule{ID: id, EffectiveFrom: feeTestDay}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestSelectFeeRule(t *testing.T) {
	phy := "11111111-1111-1111-1111-111111111111"
	item := "22222222-2222-2222-2222-222222222222"
	payer := "33333333-3333-3333-3333-333333333333"
	ctx := feeCtx{PhysicianID: phy, ServiceItemID: item, ItemType: "consultation", PayerID: payer, PerformerRole: "primary", ServiceDate: feeTestDay}

	t.Run("physician_item_beats_all", func(t *testing.T) {
		rules := []feeRule{
			mkRule("merchant", nil),
			mkRule("item_only", func(r *feeRule) { r.ServiceItemID = item }),
			mkRule("physician_only", func(r *feeRule) { r.PhysicianID = phy }),
			mkRule("physician_item", func(r *feeRule) { r.PhysicianID = phy; r.ServiceItemID = item }),
		}
		got, ok := selectFeeRule(rules, ctx)
		if !ok || got.ID != "physician_item" {
			t.Fatalf("want physician_item, got %v ok=%v", got.ID, ok)
		}
	})

	t.Run("physician_item_type_beats_physician", func(t *testing.T) {
		rules := []feeRule{
			mkRule("physician_only", func(r *feeRule) { r.PhysicianID = phy }),
			mkRule("physician_item_type", func(r *feeRule) { r.PhysicianID = phy; r.ItemType = "consultation" }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "physician_item_type" { // 40 vs 30
			t.Fatalf("want physician_item_type, got %s", got.ID)
		}
	})

	t.Run("item_beats_item_type", func(t *testing.T) {
		rules := []feeRule{
			mkRule("item_type_only", func(r *feeRule) { r.ItemType = "consultation" }),
			mkRule("item_only", func(r *feeRule) { r.ServiceItemID = item }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "item_only" { // 20 vs 10
			t.Fatalf("want item_only, got %s", got.ID)
		}
	})

	t.Run("item_type_beats_merchant", func(t *testing.T) {
		rules := []feeRule{
			mkRule("merchant", nil),
			mkRule("item_type_only", func(r *feeRule) { r.ItemType = "consultation" }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "item_type_only" { // 10 vs 0
			t.Fatalf("want item_type_only, got %s", got.ID)
		}
	})

	t.Run("payer_bonus", func(t *testing.T) {
		rules := []feeRule{
			mkRule("physician", func(r *feeRule) { r.PhysicianID = phy }),
			mkRule("physician_payer", func(r *feeRule) { r.PhysicianID = phy; r.PayerID = payer }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "physician_payer" { // 30+2 vs 30
			t.Fatalf("want physician_payer, got %s", got.ID)
		}
	})

	t.Run("role_bonus", func(t *testing.T) {
		rules := []feeRule{
			mkRule("physician", func(r *feeRule) { r.PhysicianID = phy }),
			mkRule("physician_role", func(r *feeRule) { r.PhysicianID = phy; r.PerformerRole = "primary" }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "physician_role" { // 30+1 vs 30
			t.Fatalf("want physician_role, got %s", got.ID)
		}
	})

	t.Run("non_matching_rules_excluded", func(t *testing.T) {
		rules := []feeRule{
			mkRule("other_physician", func(r *feeRule) { r.PhysicianID = "99999999-9999-9999-9999-999999999999" }),
			mkRule("merchant", nil),
		}
		got, ok := selectFeeRule(rules, ctx)
		if !ok || got.ID != "merchant" {
			t.Fatalf("want merchant, got %v ok=%v", got.ID, ok)
		}
	})

	t.Run("no_rules", func(t *testing.T) {
		if _, ok := selectFeeRule(nil, ctx); ok {
			t.Fatal("want ok=false for empty rules")
		}
	})

	t.Run("window_expired_rule_excluded", func(t *testing.T) {
		expired := feeTestDay.AddDate(0, 0, -1)
		rules := []feeRule{
			mkRule("expired", func(r *feeRule) { r.EffectiveTo = &expired }),
		}
		// service date 2024-03-01 > effective_to 2024-02-29 → excluded.
		if _, ok := selectFeeRule(rules, ctx); ok {
			t.Fatal("want ok=false for expired rule")
		}
	})

	t.Run("window_future_rule_excluded", func(t *testing.T) {
		future := feeTestDay.AddDate(0, 0, 1)
		rules := []feeRule{
			mkRule("future", func(r *feeRule) { r.EffectiveFrom = future }),
		}
		// service date 2024-03-01 < effective_from 2024-03-02 → excluded.
		if _, ok := selectFeeRule(rules, ctx); ok {
			t.Fatal("want ok=false for future rule")
		}
	})

	t.Run("window_boundaries_inclusive", func(t *testing.T) {
		// service date == effective_from → included.
		fromRules := []feeRule{mkRule("from_today", nil)} // effective_from = feeTestDay = ServiceDate
		if got, ok := selectFeeRule(fromRules, ctx); !ok || got.ID != "from_today" {
			t.Fatalf("want from_today at effective_from, got %v ok=%v", got.ID, ok)
		}
		// service date == effective_to → included.
		end := feeTestDay
		toRules := []feeRule{
			mkRule("ends_today", func(r *feeRule) { r.EffectiveTo = &end }),
		}
		if got, ok := selectFeeRule(toRules, ctx); !ok || got.ID != "ends_today" {
			t.Fatalf("want ends_today at effective_to, got %v ok=%v", got.ID, ok)
		}
	})

	t.Run("tie_newest_effective_from_wins", func(t *testing.T) {
		rules := []feeRule{
			mkRule("old", func(r *feeRule) { r.PhysicianID = phy; r.EffectiveFrom = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) }),
			mkRule("new", func(r *feeRule) { r.PhysicianID = phy; r.EffectiveFrom = time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC) }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "new" {
			t.Fatalf("want newest rule, got %s", got.ID)
		}
	})

	t.Run("tie_same_date_smallest_id_wins", func(t *testing.T) {
		rules := []feeRule{
			mkRule("b", func(r *feeRule) { r.PhysicianID = phy }),
			mkRule("a", func(r *feeRule) { r.PhysicianID = phy }),
		}
		got, _ := selectFeeRule(rules, ctx)
		if got.ID != "a" {
			t.Fatalf("want smallest id a, got %s", got.ID)
		}
	})
}

func TestComputeFee(t *testing.T) {
	// line: total 100.000; MEDIS 40.000 + SARANA 60.000 (all parties combined).
	line := feeLine{
		AccountID:  "acc-1",
		Quantity:   2,
		TotalCents: 10_000_000,
		Components: map[string]int64{"MEDIS": 4_000_000, "SARANA": 6_000_000},
	}

	t.Run("percent_of_component_uses_medis_gross", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "percent_of_component"; r.ComponentCode = "MEDIS"; r.PercentBP = 4000 })
		gross, fee := computeFee(rule, line)
		if gross != 4_000_000 { // MEDIS portion, not the 100.000 total
			t.Fatalf("gross = %d, want 4000000", gross)
		}
		if fee != 1_600_000 { // 40% × 40.000 = 16.000
			t.Fatalf("fee = %d, want 1600000", fee)
		}
	})

	t.Run("gross_falls_back_to_total_without_medis", func(t *testing.T) {
		noMedis := feeLine{TotalCents: 10_000_000, Components: map[string]int64{"SARANA": 10_000_000}}
		rule := mkRule("r", func(r *feeRule) { r.Basis = "percent_of_component"; r.ComponentCode = "MEDIS"; r.PercentBP = 4000 })
		gross, fee := computeFee(rule, noMedis)
		if gross != 10_000_000 {
			t.Fatalf("gross = %d, want 10000000", gross)
		}
		if fee != 0 { // no MEDIS component portion to apply the percent to
			t.Fatalf("fee = %d, want 0", fee)
		}
	})

	t.Run("percent_of_total", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "percent_of_total"; r.PercentBP = 2500 })
		gross, fee := computeFee(rule, line)
		if gross != 4_000_000 {
			t.Fatalf("gross = %d, want 4000000", gross)
		}
		if fee != 2_500_000 { // 25% × 100.000 = 25.000
			t.Fatalf("fee = %d, want 2500000", fee)
		}
	})

	t.Run("percent_half_up_to_cent", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "percent_of_total"; r.PercentBP = 5000 }) // 50%
		_, fee := computeFee(rule, feeLine{TotalCents: 33})
		if fee != 17 { // 33 × 0.5 = 16.5 → 17 (half-up)
			t.Fatalf("fee = %d, want 17", fee)
		}
	})

	t.Run("fixed_per_item_multiplied_by_quantity", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "fixed_per_item"; r.FixedCents = 1500 })
		_, fee := computeFee(rule, feeLine{Quantity: 2000}) // milli-units = 2 items
		if fee != 3_000 {                                   // 1.500 × 2 item = 3.000
			t.Fatalf("fee = %d, want 3000", fee)
		}
	})

	t.Run("fixed_per_item_fractional_quantity", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "fixed_per_item"; r.FixedCents = 1500 })
		_, fee := computeFee(rule, feeLine{Quantity: 2500}) // milli-units = 2.5 items
		if fee != 3_750 {                                   // 2500 × 1500 / 1000 = 3.750
			t.Fatalf("fee = %d, want 3750", fee)
		}
	})

	t.Run("fixed_per_visit_first_line_only", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "fixed_per_visit"; r.FixedCents = 5_000_000 }) // 50.000
		first := line
		first.FirstForVisit = true
		second := line
		if _, fee := computeFee(rule, first); fee != 5_000_000 {
			t.Fatalf("first visit fee = %d, want 5000000", fee)
		}
		if _, fee := computeFee(rule, second); fee != 0 {
			t.Fatalf("second visit fee = %d, want 0", fee)
		}
	})

	t.Run("capitation_package_line_has_no_fee", func(t *testing.T) {
		rule := mkRule("r", func(r *feeRule) { r.Basis = "percent_of_total"; r.PercentBP = 5000 })
		cap := line
		cap.AllCapitationOrPackage = true
		gross, fee := computeFee(rule, cap)
		if gross != 4_000_000 {
			t.Fatalf("gross = %d, want 4000000", gross)
		}
		if fee != 0 {
			t.Fatalf("fee = %d, want 0", fee)
		}
	})
}
