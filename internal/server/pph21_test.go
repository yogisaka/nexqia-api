package server

// Unit tests for the pure PPh 21 functions (spec §4.3.4). The tables are the
// seed data of migration 000070 rewritten as fixtures: TER = Lampiran
// PP 58/2023, PTKP = PMK 101/PMK.010/2016, Pasal 17 = UU 7/2021 (HPP),
// biaya jabatan = PMK 250/PMK.03/2008, partner base = 50%.

import "testing"

// rp converts a rupiah amount to cents.
func rp(rupiah int64) int64 { return rupiah * 100 }

func testTaxTables() pph21Tables {
	return pph21Tables{
		TER: []terRow{
			// Category A (TK/0, TK/1, K/0)
			{"A", rp(0), rp(5400000), 0},
			{"A", rp(5400000), rp(5650000), 25},
			{"A", rp(5650000), rp(5950000), 50},
			{"A", rp(5950000), rp(6300000), 75},
			{"A", rp(6300000), rp(6750000), 100},
			{"A", rp(6750000), rp(7500000), 125},
			{"A", rp(7500000), rp(8550000), 150},
			{"A", rp(8550000), rp(9650000), 175},
			{"A", rp(9650000), rp(10050000), 200},
			{"A", rp(10050000), rp(10350000), 225},
			{"A", rp(10350000), rp(10700000), 250},
			{"A", rp(10700000), rp(11050000), 300},
			{"A", rp(11050000), rp(11600000), 350},
			{"A", rp(11600000), rp(12500000), 400},
			{"A", rp(12500000), rp(13750000), 500},
			{"A", rp(13750000), rp(15100000), 600},
			{"A", rp(15100000), rp(16950000), 700},
			{"A", rp(16950000), rp(19750000), 800},
			{"A", rp(19750000), rp(24150000), 900},
			{"A", rp(24150000), rp(26450000), 1000},
			{"A", rp(26450000), rp(28000000), 1100},
			{"A", rp(28000000), rp(30050000), 1200},
			{"A", rp(30050000), rp(32400000), 1300},
			{"A", rp(32400000), rp(35400000), 1400},
			{"A", rp(35400000), rp(39100000), 1500},
			{"A", rp(39100000), rp(43850000), 1600},
			{"A", rp(43850000), rp(47800000), 1700},
			{"A", rp(47800000), rp(51400000), 1800},
			{"A", rp(51400000), rp(56300000), 1900},
			{"A", rp(56300000), rp(62200000), 2000},
			{"A", rp(62200000), rp(68600000), 2100},
			{"A", rp(68600000), rp(77500000), 2200},
			{"A", rp(77500000), rp(89000000), 2300},
			{"A", rp(89000000), rp(103000000), 2400},
			{"A", rp(103000000), rp(125000000), 2500},
			{"A", rp(125000000), rp(157000000), 2600},
			{"A", rp(157000000), rp(206000000), 2700},
			{"A", rp(206000000), rp(337000000), 2800},
			{"A", rp(337000000), rp(454000000), 2900},
			{"A", rp(454000000), rp(550000000), 3000},
			{"A", rp(550000000), rp(695000000), 3100},
			{"A", rp(695000000), rp(910000000), 3200},
			{"A", rp(910000000), rp(1400000000), 3300},
			{"A", rp(1400000000), -1, 3400},
			// Category B (TK/2, TK/3, K/1, K/2)
			{"B", rp(0), rp(6200000), 0},
			{"B", rp(6200000), rp(6500000), 25},
			{"B", rp(6500000), rp(6850000), 50},
			{"B", rp(6850000), rp(7300000), 75},
			{"B", rp(7300000), rp(9200000), 100},
			{"B", rp(9200000), rp(10750000), 150},
			{"B", rp(10750000), rp(11250000), 200},
			{"B", rp(11250000), rp(11600000), 250},
			{"B", rp(11600000), rp(12600000), 300},
			{"B", rp(12600000), rp(13600000), 400},
			{"B", rp(13600000), rp(14950000), 500},
			{"B", rp(14950000), rp(16400000), 600},
			{"B", rp(16400000), rp(18450000), 700},
			{"B", rp(18450000), rp(21850000), 800},
			{"B", rp(21850000), rp(26000000), 900},
			{"B", rp(26000000), rp(27700000), 1000},
			{"B", rp(27700000), rp(29350000), 1100},
			{"B", rp(29350000), rp(31450000), 1200},
			{"B", rp(31450000), rp(33950000), 1300},
			{"B", rp(33950000), rp(37100000), 1400},
			{"B", rp(37100000), rp(41100000), 1500},
			{"B", rp(41100000), rp(45800000), 1600},
			{"B", rp(45800000), rp(49500000), 1700},
			{"B", rp(49500000), rp(53800000), 1800},
			{"B", rp(53800000), rp(58500000), 1900},
			{"B", rp(58500000), rp(64000000), 2000},
			{"B", rp(64000000), rp(71000000), 2100},
			{"B", rp(71000000), rp(80000000), 2200},
			{"B", rp(80000000), rp(93000000), 2300},
			{"B", rp(93000000), rp(109000000), 2400},
			{"B", rp(109000000), rp(129000000), 2500},
			{"B", rp(129000000), rp(163000000), 2600},
			{"B", rp(163000000), rp(211000000), 2700},
			{"B", rp(211000000), rp(374000000), 2800},
			{"B", rp(374000000), rp(459000000), 2900},
			{"B", rp(459000000), rp(555000000), 3000},
			{"B", rp(555000000), rp(704000000), 3100},
			{"B", rp(704000000), rp(957000000), 3200},
			{"B", rp(957000000), rp(1405000000), 3300},
			{"B", rp(1405000000), -1, 3400},
			// Category C (K/3)
			{"C", rp(0), rp(6600000), 0},
			{"C", rp(6600000), rp(6950000), 25},
			{"C", rp(6950000), rp(7350000), 50},
			{"C", rp(7350000), rp(7800000), 75},
			{"C", rp(7800000), rp(8850000), 100},
			{"C", rp(8850000), rp(9800000), 125},
			{"C", rp(9800000), rp(10950000), 150},
			{"C", rp(10950000), rp(11200000), 175},
			{"C", rp(11200000), rp(12050000), 200},
			{"C", rp(12050000), rp(12950000), 300},
			{"C", rp(12950000), rp(14150000), 400},
			{"C", rp(14150000), rp(15550000), 500},
			{"C", rp(15550000), rp(17050000), 600},
			{"C", rp(17050000), rp(19500000), 700},
			{"C", rp(19500000), rp(22700000), 800},
			{"C", rp(22700000), rp(26600000), 900},
			{"C", rp(26600000), rp(28100000), 1000},
			{"C", rp(28100000), rp(30100000), 1100},
			{"C", rp(30100000), rp(32600000), 1200},
			{"C", rp(32600000), rp(35400000), 1300},
			{"C", rp(35400000), rp(38900000), 1400},
			{"C", rp(38900000), rp(43000000), 1500},
			{"C", rp(43000000), rp(47400000), 1600},
			{"C", rp(47400000), rp(51200000), 1700},
			{"C", rp(51200000), rp(55800000), 1800},
			{"C", rp(55800000), rp(60400000), 1900},
			{"C", rp(60400000), rp(66700000), 2000},
			{"C", rp(66700000), rp(74500000), 2100},
			{"C", rp(74500000), rp(83200000), 2200},
			{"C", rp(83200000), rp(95600000), 2300},
			{"C", rp(95600000), rp(110000000), 2400},
			{"C", rp(110000000), rp(134000000), 2500},
			{"C", rp(134000000), rp(169000000), 2600},
			{"C", rp(169000000), rp(221000000), 2700},
			{"C", rp(221000000), rp(390000000), 2800},
			{"C", rp(390000000), rp(463000000), 2900},
			{"C", rp(463000000), rp(561000000), 3000},
			{"C", rp(561000000), rp(709000000), 3100},
			{"C", rp(709000000), rp(965000000), 3200},
			{"C", rp(965000000), rp(1419000000), 3300},
			{"C", rp(1419000000), -1, 3400},
		},
		PTKP: map[string]int64{
			"TK/0": rp(54000000), "TK/1": rp(58500000),
			"TK/2": rp(63000000), "TK/3": rp(67500000),
			"K/0": rp(58500000), "K/1": rp(63000000),
			"K/2": rp(67500000), "K/3": rp(72000000),
		},
		Pasal17: []taxBracket{
			{rp(60000000), 500},
			{rp(250000000), 1500},
			{rp(500000000), 2500},
			{rp(5000000000), 3000},
			{-1, 3500},
		},
		BiayaJabatanBP:        500, // 5%
		BiayaJabatanMaxAnnual: rp(6000000),
		PartnerBaseBP:         5000, // 50%
	}
}

func TestPPh21(t *testing.T) {
	tbl := testTaxTables()

	t.Run("partner_small", func(t *testing.T) {
		base, tax := pph21Partner(rp(10000000), tbl)
		if base != rp(5000000) {
			t.Fatalf("base = %d cents, want %d", base, rp(5000000))
		}
		if tax != rp(250000) { // 5% × 5.000.000
			t.Fatalf("tax = %d cents, want %d", tax, rp(250000))
		}
	})

	t.Run("partner_two_brackets", func(t *testing.T) {
		// base 100.000.000: 60jt×5% + 40jt×15% = 3jt + 6jt = 9.000.000.
		_, tax := pph21Partner(rp(200000000), tbl)
		if tax != rp(9000000) {
			t.Fatalf("tax = %d cents, want %d", tax, rp(9000000))
		}
	})

	t.Run("ter_a_zero", func(t *testing.T) {
		cat, rateBP, tax := pph21TERMonthly(rp(5400000), "TK/0", tbl)
		if cat != "A" || rateBP != 0 || tax != 0 {
			t.Fatalf("got cat=%s rate=%d tax=%d, want A/0/0", cat, rateBP, tax)
		}
	})

	t.Run("ter_a_first", func(t *testing.T) {
		// 5.400.000 < x ≤ 5.650.000 → 0,25% → 13.500.
		cat, rateBP, tax := pph21TERMonthly(rp(5400001), "TK/0", tbl)
		if cat != "A" || rateBP != 25 {
			t.Fatalf("got cat=%s rate=%d, want A/25", cat, rateBP)
		}
		if tax != rp(13500) {
			t.Fatalf("tax = %d cents, want %d", tax, rp(13500))
		}
	})

	t.Run("ter_b", func(t *testing.T) {
		// K/1 → B; 9.200.000 < 10.000.000 ≤ 10.750.000 → 1,5% → 150.000.
		cat, rateBP, tax := pph21TERMonthly(rp(10000000), "K/1", tbl)
		if cat != "B" || rateBP != 150 {
			t.Fatalf("got cat=%s rate=%d, want B/150", cat, rateBP)
		}
		if tax != rp(150000) {
			t.Fatalf("tax = %d cents, want %d", tax, rp(150000))
		}
	})

	t.Run("ter_c_top", func(t *testing.T) {
		// K/3 → C; above 1.419.000.000 → 34%.
		_, rateBP, tax := pph21TERMonthly(rp(1500000000), "K/3", tbl)
		if rateBP != 3400 {
			t.Fatalf("rate = %d, want 3400", rateBP)
		}
		if tax != rp(1500000000)*3400/10000 { // 510.000.000
			t.Fatalf("tax = %d cents, want %d", tax, rp(510000000))
		}
	})

	t.Run("category_map", func(t *testing.T) {
		cases := map[string]string{
			"TK/0": "A", "TK/1": "A", "K/0": "A",
			"TK/2": "B", "TK/3": "B", "K/1": "B", "K/2": "B",
			"K/3": "C",
		}
		for ptkp, want := range cases {
			if got := terCategory(ptkp); got != want {
				t.Errorf("terCategory(%s) = %s, want %s", ptkp, got, want)
			}
		}
	})

	t.Run("december", func(t *testing.T) {
		// K/0, annual gross 120.000.000, Jan–Nov tax 1.500.000:
		// biaya jabatan min(5%×120jt, 6jt) = 6.000.000; PTKP 58.500.000;
		// PKP 55.500.000; annual tax 5% = 2.775.000; Dec = 1.275.000.
		pkp, annualTax, decTax := pph21December(rp(120000000), "K/0", rp(1500000), tbl)
		if pkp != rp(55500000) {
			t.Fatalf("pkp = %d cents, want %d", pkp, rp(55500000))
		}
		if annualTax != rp(2775000) {
			t.Fatalf("annualTax = %d cents, want %d", annualTax, rp(2775000))
		}
		if decTax != rp(1275000) {
			t.Fatalf("decTax = %d cents, want %d", decTax, rp(1275000))
		}
	})

	t.Run("december_overpaid", func(t *testing.T) {
		// Same December base but Jan–Nov already withheld 3.000.000 →
		// December = 2.775.000 − 3.000.000 = −225.000.
		_, _, decTax := pph21December(rp(120000000), "K/0", rp(3000000), tbl)
		if decTax != -rp(225000) {
			t.Fatalf("decTax = %d cents, want %d", decTax, -rp(225000))
		}
	})

	t.Run("pkp_floored_to_thousand_rupiah", func(t *testing.T) {
		// 70.000.123 − 3.500.006,15 (5%, below the 6jt cap) − 54.000.000
		// (TK/0 PTKP) = 12.500.116,85 → floored to 12.500.000.
		pkp, _, _ := pph21December(rp(70000123), "TK/0", 0, tbl)
		if pkp != rp(12500000) {
			t.Fatalf("pkp = %d cents, want %d", pkp, rp(12500000))
		}
	})
}
