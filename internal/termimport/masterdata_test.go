package termimport

import (
	"fmt"
	"strings"
	"testing"
)

// wilayahMini builds a valid mini wilayah SQL dump with one full chain
// (provinsi 11 → kab 11.01 → kec 11.01.01 → desa 11.01.01.2001) plus the
// extra rows given as extra (code, name) pairs.
func wilayahMini(extra ...string) string {
	base := []string{
		"INSERT INTO wilayah VALUES ('11','Aceh');",
		"INSERT INTO wilayah VALUES ('11.01','Kabupaten Aceh Selatan');",
		"INSERT INTO wilayah VALUES ('11.01.01','Bakongan');",
		"INSERT INTO wilayah VALUES ('11.01.01.2001','Keude Bakongan');",
	}
	return strings.Join(append(base, extra...), "\n")
}

// expectWilayahCount is the custom expectation used by mini happy-path tests.
type expectWilayahCount struct{ prov, kab, kec, desa int }

func (e expectWilayahCount) want() [4]int {
	return [4]int{e.prov, e.kab, e.kec, e.desa}
}

// parseWilayahMini runs the internal parser (custom expectations instead of
// ExpectedWilayah) against a mini dump whose counts must equal the custom
// expectation; level-structure and parent checks still run.
func parseWilayahMini(t *testing.T, dump string, want expectWilayahCount) ParseResult {
	t.Helper()
	got := countLevels(dump)
	if got != want.want() {
		t.Fatalf("mini dump levels %v, want %v", got, want.want())
	}
	res, err := parseWilayahCounts(strings.NewReader(dump), want.want())
	if err != nil {
		t.Fatalf("parseWilayahCounts: %v", err)
	}
	return res
}

// countLevels parses a dump purely to compute per-level counts (mini dumps
// are expected to be internally consistent).
func countLevels(dump string) [4]int {
	var counts [4]int
	for _, m := range wilayahTupleRe.FindAllStringSubmatch(dump, -1) {
		counts[len(strings.Split(m[1], "."))-1]++
	}
	return counts
}

func TestParseWilayahSQL(t *testing.T) {
	t.Run("happy mini full chain", func(t *testing.T) {
		res := parseWilayahMini(t, wilayahMini(
			"INSERT INTO wilayah VALUES ('11.01.02','Kluet Utara');",
			"INSERT INTO wilayah VALUES ('11.01.02.2003','Lawe Kingup');",
		), expectWilayahCount{prov: 1, kab: 1, kec: 2, desa: 2})
		if len(res.Concepts) != 6 {
			t.Fatalf("got %d concepts, want 6", len(res.Concepts))
		}
		// Contract: codes stored as-is with dots.
		if res.Concepts[3].Code != "11.01.01.2001" || res.Concepts[3].Display != "Keude Bakongan" || !res.Concepts[3].Selectable {
			t.Fatalf("concept[3] = %+v", res.Concepts[3])
		}
		if res.Concepts[0].Properties != nil {
			t.Fatalf("Properties must be nil, got %+v", res.Concepts[0].Properties)
		}
		// Edge: parent = code without last segment.
		wantEdges := map[string]string{
			"11.01":         "11",
			"11.01.01":      "11.01",
			"11.01.02":      "11.01",
			"11.01.01.2001": "11.01.01",
			"11.01.02.2003": "11.01.02",
		}
		if len(res.Edges) != len(wantEdges) {
			t.Fatalf("got %d edges, want %d", len(res.Edges), len(wantEdges))
		}
		for _, e := range res.Edges {
			if wantEdges[e.Child] != e.Parent {
				t.Fatalf("edge %v, want parent %s", e, wantEdges[e.Child])
			}
		}
		// Skipped holds exactly one kab/kota report line.
		if len(res.Skipped) != 1 || !strings.HasPrefix(res.Skipped[0], "kab/kota: 1 KAB / 0 KOTA") {
			t.Fatalf("Skipped = %v", res.Skipped)
		}
	})

	t.Run("happy mini escaped quote display", func(t *testing.T) {
		dump := strings.ReplaceAll(wilayahMini(), "'Kabupaten Aceh Selatan'", "'Kabupaten Aceh ''Selatan'''")
		res := parseWilayahMini(t, dump, expectWilayahCount{prov: 1, kab: 1, kec: 1, desa: 1})
		if res.Concepts[1].Display != "Kabupaten Aceh 'Selatan'" {
			t.Fatalf("unescape failed: %q", res.Concepts[1].Display)
		}
	})

	t.Run("fail vs ExpectedWilayah constants", func(t *testing.T) {
		_, err := ParseWilayahSQL(strings.NewReader(wilayahMini()))
		if err == nil {
			t.Fatal("mini dump must fail against ExpectedWilayah")
		}
		// Actual counts 1/1/1/1 vs constants 38/514/7285/83762; error must
		// report actual vs expected per level.
		if !strings.Contains(err.Error(), fmt.Sprintf("aktual %d", 38)) && !strings.Contains(err.Error(), "38") {
			t.Fatalf("error must mention expected provinsi count 38: %v", err)
		}
	})

	t.Run("bad segment structure 11.1", func(t *testing.T) {
		dump := strings.ReplaceAll(wilayahMini(), "'11.01'", "'11.1'")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "wilayah:") || !strings.Contains(err.Error(), "11.1") {
			t.Fatalf("err = %v, want wilayah: struktur segmen salah untuk 11.1", err)
		}
	})

	t.Run("digit out of structure 11.01.010", func(t *testing.T) {
		dump := strings.ReplaceAll(wilayahMini(), "'11.01.01'", "'11.01.010'")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "11.01.010") {
			t.Fatalf("err = %v, want structure error for 11.01.010", err)
		}
	})

	t.Run("level count mismatch", func(t *testing.T) {
		// 4-segment code with only 3 segments total path missing: structure
		// fine, but parent lookup would fail; use extra provinsi to trigger
		// level-count mismatch after all rows parse.
		dump := wilayahMini("INSERT INTO wilayah VALUES ('12','Sumatera Utara');")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "jumlah level") || !strings.Contains(err.Error(), "provinsi: aktual 2, ekspektasi 38") {
			t.Fatalf("err = %v, want jumlah level provinsi mismatch", err)
		}
	})

	t.Run("duplicate code", func(t *testing.T) {
		dump := wilayahMini("INSERT INTO wilayah VALUES ('11.01.01','Bakongan lagi');")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "duplikat 11.01.01") {
			t.Fatalf("err = %v, want duplicate 11.01.01", err)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		dump := strings.ReplaceAll(wilayahMini(), "'11.01','Kabupaten Aceh Selatan'", "'11.02','Kabupaten Aceh Timur'")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "parent 11.01 tidak ditemukan untuk kode 11.01.01") {
			t.Fatalf("err = %v, want missing parent 11", err)
		}
	})

	t.Run("empty display", func(t *testing.T) {
		dump := strings.ReplaceAll(wilayahMini(), "'Bakongan'", "''")
		_, err := ParseWilayahSQL(strings.NewReader(dump))
		if err == nil || !strings.Contains(err.Error(), "display kosong") {
			t.Fatalf("err = %v, want empty display", err)
		}
	})

	t.Run("zero tuples", func(t *testing.T) {
		_, err := ParseWilayahSQL(strings.NewReader("SELECT 1; -- no data"))
		if err == nil || !strings.Contains(err.Error(), "0 tuple ditemukan") {
			t.Fatalf("err = %v, want zero tuples", err)
		}
	})
}

func TestParseISO3166JSON(t *testing.T) {
	entry := func(a2, name string) string {
		return fmt.Sprintf(`{"name":%q,"alpha-2":%q,"alpha-3":"XXX"}`, name, a2)
	}
	full := func(entries ...string) string { return "[" + strings.Join(entries, ",") + "]" }

	// Build a valid 249-entry list: 248 unique uppercase fillers + Indonesia
	// appended last (filler "ID" is skipped so it stays unique).
	validList := func(indonesiaName string) string {
		items := make([]string, 0, 249)
		for i := 0; len(items) < 248; i++ {
			a2 := fmt.Sprintf("%c%c", 'A'+i/26, 'A'+i%26)
			if a2 == "ID" {
				continue
			}
			items = append(items, entry(a2, "Country "+a2))
		}
		items = append(items, entry("ID", indonesiaName))
		return full(items...)
	}

	t.Run("happy 249 entries", func(t *testing.T) {
		res, err := ParseISO3166JSON(strings.NewReader(validList("Indonesia")))
		if err != nil {
			t.Fatalf("ParseISO3166JSON: %v", err)
		}
		if len(res.Concepts) != 249 {
			t.Fatalf("got %d concepts, want 249", len(res.Concepts))
		}
		last := res.Concepts[248]
		if last.Code != "ID" || last.Display != "Indonesia" || !last.Selectable || last.Properties != nil {
			t.Fatalf("last concept = %+v", last)
		}
		if len(res.Edges) != 0 || len(res.Skipped) != 0 {
			t.Fatalf("edges/skipped must be empty, got %v/%v", res.Edges, res.Skipped)
		}
	})

	t.Run("not 249 entries", func(t *testing.T) {
		_, err := ParseISO3166JSON(strings.NewReader(full(entry("ID", "Indonesia"))))
		if err == nil || !strings.Contains(err.Error(), "249") {
			t.Fatalf("err = %v, want count mismatch vs 249", err)
		}
	})

	t.Run("lowercase alpha-2", func(t *testing.T) {
		list := strings.Replace(validList("Indonesia"), `"id"`, `"zz"`, 1) // no-op safety
		list = strings.Replace(list, `"ID"`, `"id"`, 1)
		_, err := ParseISO3166JSON(strings.NewReader(list))
		if err == nil || !strings.Contains(err.Error(), "iso3166: alpha-2 tidak valid") {
			t.Fatalf("err = %v, want invalid alpha-2", err)
		}
	})

	t.Run("duplicate alpha-2", func(t *testing.T) {
		list := validList("Indonesia")
		list = strings.Replace(list, `"Country AA"`, `"Indonesia"`, 1)      // rename AA entry
		list = strings.Replace(list, `"alpha-2":"AA"`, `"alpha-2":"ID"`, 1) // second copy of ID
		_, err := ParseISO3166JSON(strings.NewReader(list))
		if err == nil || !strings.Contains(err.Error(), "duplikat ID") {
			t.Fatalf("err = %v, want duplicate ID", err)
		}
	})

	t.Run("ID not named Indonesia", func(t *testing.T) {
		_, err := ParseISO3166JSON(strings.NewReader(validList("Nusantara")))
		if err == nil || !strings.Contains(err.Error(), "Indonesia") {
			t.Fatalf("err = %v, want Indonesia name mismatch", err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		list := validList("Indonesia")
		list = strings.Replace(list, `"Country AA"`, `""`, 1)
		_, err := ParseISO3166JSON(strings.NewReader(list))
		if err == nil || !strings.Contains(err.Error(), "nama kosong") {
			t.Fatalf("err = %v, want empty name", err)
		}
	})

	t.Run("decode error not array", func(t *testing.T) {
		_, err := ParseISO3166JSON(strings.NewReader(`{"name":"x"}`))
		if err == nil || !strings.Contains(err.Error(), "iso3166:") {
			t.Fatalf("err = %v, want decode error", err)
		}
	})
}
