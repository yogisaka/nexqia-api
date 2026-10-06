package termimport

import (
	"strings"
	"testing"
)

func TestStemCode(t *testing.T) {
	for in, want := range map[string]string{"1A00": "1A00", "1A00&XN8P1": "1A00", "NE84/PB28": "NE84", "XA0001": "XA0001"} {
		if got := StemCode(in); got != want {
			t.Fatalf("StemCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMap10To11(t *testing.T) {
	multi := "\ufeff10ClassKind\tDepth\ticd10Code\ticd10Chapter\ticd10Title\t11ClassKind\tDepth\tICD-11 Foundation URI\tLinearization (release) URI\ticd11Code\ticd11Chapter\ticd11Title\t2026-Jan-17\r\n" +
		"chapter\t1\tI\tI\tInfectious\tcategory\t1\tf\tl\t1H0Z\t01\tInfection\r\n" +
		"category\t3\tA00.0\tI\tCholera O1\tcategory\t3\tf\tl\t1A00&XN8P1\t01\tCholera\r\n" +
		"category\t3\tA00.9\tI\tCholera\tcategory\t3\tf\tl\t1A00\t01\tCholera\r\n" +
		"category\t3\tA00.9\tI\tCholera\tcategory\t3\tf\tl\t1A01\t01\tOther\r\n" +
		"category\t3\tA00.9\tI\tCholera\tcategory\t3\tf\tl\t1A00\t01\tCholera\r\n" +
		"category\t3\tA09\tI\tNo target\tcategory\t3\tf\tl\t\t\t\r\n"
	one := "10ClassKind\t10DepthInKind\ticd10Code\ticd10Chapter\ticd10Title\t11ClassKind\t11DepthInKind\tICD-11 FoundationURI\tLinearization (releaseURI)\ticd11Code\ticd11Chapter\ticd11Title\t2026-Jan-17\n" +
		"category\t3\tA00.9\tI\tCholera\tcategory\t3\tf\tl\t1A00\t01\tCholera\n"
	rows, err := ParseMap10To11(strings.NewReader(multi), strings.NewReader(one))
	if err != nil {
		t.Fatalf("ParseMap10To11: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SourceCode+">"+r.TargetCode] = r.Preferred
	}
	want := map[string]bool{"A00.0>1A00&XN8P1": false, "A00.9>1A00": true, "A00.9>1A01": false}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %v (chapter row, empty target and duplicate dropped)", rows, want)
	}
	for k, pref := range want {
		if p, ok := got[k]; !ok || p != pref {
			t.Fatalf("row %s preferred=%v present=%v, want preferred=%v", k, p, ok, pref)
		}
	}
}

func TestParseMap11To10(t *testing.T) {
	in := "Linearization (release) URI\ticd11Code\ticd11Chapter\ticd11Title\ticd10Code\ticd10Chapter\ticd10Title\t2026-Jan-17\n" +
		"l\t1A00\t01\tCholera\tA00.9\tI\tCholera\n" +
		"l\t\t01\tNo code\tI\tI\tChapter\n" +
		"l\t1A00\t01\tCholera\tA00.9\tI\tCholera\n"
	rows, err := ParseMap11To10(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseMap11To10: %v", err)
	}
	if len(rows) != 1 || rows[0] != (MapRow{SourceCode: "1A00", TargetCode: "A00.9", Preferred: true}) {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestParseMapBrokenHeaders(t *testing.T) {
	if _, err := ParseMap10To11(strings.NewReader("foo\tbar\n1\t2\n"), strings.NewReader("icd10Code\ticd11Code\nA00.9\t1A00\n")); err == nil {
		t.Fatal("10To11 with missing 10ClassKind/icd10Code/icd11Code headers must error")
	}
	if _, err := ParseMap10To11(strings.NewReader("10ClassKind\ticd10Code\ticd11Code\ncategory\tA00.9\t1A00\n"), strings.NewReader("foo\tbar\n1\t2\n")); err == nil {
		t.Fatal("10To11 one-category file with missing icd10Code/icd11Code headers must error")
	}
	if _, err := ParseMap11To10(strings.NewReader("icd11Code\tfoo\n1A00\t1\n")); err == nil {
		t.Fatal("11To10 with missing icd10Code header must error")
	}
}
