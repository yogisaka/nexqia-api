package termimport

import (
	"strings"
	"testing"
)

// icd11Header is the 20-column header of SimpleTabulation (last = Version).
var icd11Header = strings.Join([]string{"Foundation URI", "Linearization URI", "Code", "BlockId", "Title", "ClassKind",
	"DepthInKind", "IsResidual", "ChapterNo", "BrowserLink", "isLeaf", "Primary tabulation", "Grouping1", "Grouping2",
	"Grouping3", "Grouping4", "Grouping5", "CodingNote", "Parent", "Version:2026 Jan 17 - 05:30 UTC"}, "\t")

// icd11Row builds one 19-column data row.
func icd11Row(found, lin, code, block, title, kind, depth, residual, chapter, leaf, note, parent string) string {
	return strings.Join([]string{found, lin, code, block, title, kind, depth, residual, chapter, "", leaf, "",
		"", "", "", "", "", note, parent}, "\t")
}

const (
	fCh01 = "http://id.who.int/icd/entity/1"
	fBlk  = "http://id.who.int/icd/entity/2"
	fChol = "http://id.who.int/icd/entity/3"
	fChX  = "http://id.who.int/icd/entity/9"
	lin   = "http://id.who.int/icd/release/11/mms/"
)

// bom is the UTF-8 byte order mark U+FEFF (gofmt rejects a raw BOM in source).
var bom = string(rune(65279))

func TestParseICD11(t *testing.T) {
	// The cholera entry's CodingNote spans two physical lines; its Parent
	// column sits on the continuation line. The residual row starts with an
	// EMPTY Foundation URI (leading tab) and must NOT be read as continuation.
	cholera := icd11Row(fChol, lin+"3", "1A00", "", `"- - Cholera"`, "category", "3", "False", "01", "True",
		"Use additional code", "")
	cholera = strings.TrimSuffix(cholera, "\t") // Parent moves to the continuation line
	input := bom + strings.Join([]string{
		icd11Header,
		icd11Row(fCh01, lin+"1", "", "", `"Certain infectious or parasitic diseases"`, "chapter", "1", "False", "01", "False", "", ""),
		icd11Row(fBlk, lin+"2", "", "BlockL1-1A0", `"- Intestinal infectious diseases"`, "block", "1", "False", "01", "False", "", fCh01),
		cholera,
		"second note line\t" + fBlk,
		icd11Row("", lin+"2/unspecified", "1A0Z", "", `"- - Intestinal infections, unspecified"`, "category", "3", "True", "01", "True", "", fBlk),
		"",
		icd11Row(fChX, lin+"9", "", "", `"Extension Codes"`, "chapter", "1", "False", "X", "False", "", ""),
		icd11Row("http://id.who.int/icd/entity/10", lin+"10", "XA0001", "", `"- Left"`, "category", "1", "False", "X", "True", "", fChX),
	}, "\r\n")

	res, err := ParseICD11(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseICD11: %v", err)
	}
	byCode := map[string]Concept{}
	for _, c := range res.Concepts {
		byCode[c.Code] = c
	}
	if len(res.Concepts) != 6 {
		t.Fatalf("concepts = %d, want 6 (the residual row is an entry, not a note): %+v", len(res.Concepts), res.Concepts)
	}
	want := map[string]bool{"01": false, "BlockL1-1A0": false, "1A00": true, "1A0Z": true, "X": false, "XA0001": false}
	for code, sel := range want {
		c, ok := byCode[code]
		if !ok {
			t.Fatalf("concept %s missing", code)
		}
		if c.Selectable != sel {
			t.Fatalf("%s selectable = %v, want %v", code, c.Selectable, sel)
		}
		if c.Properties["linearization_uri"] == "" {
			t.Fatalf("%s has no linearization_uri", code)
		}
	}
	if got := byCode["1A00"].Display; got != "Cholera" {
		t.Fatalf("display = %q, want depth markers and quotes stripped", got)
	}
	if got := byCode["1A00"].Properties["coding_note"]; got != "Use additional code\nsecond note line" {
		t.Fatalf("coding_note = %q", got)
	}
	if byCode["1A0Z"].Properties["foundation_uri"] != "" || byCode["1A0Z"].Properties["is_residual"] != true {
		t.Fatalf("residual properties = %+v", byCode["1A0Z"].Properties)
	}
	if byCode["XA0001"].Properties["is_extension"] != true || byCode["1A00"].Properties["is_extension"] != false {
		t.Fatal("is_extension must follow chapter X")
	}
	edges := map[Edge]bool{}
	for _, e := range res.Edges {
		edges[e] = true
	}
	for _, e := range []Edge{{"01", "BlockL1-1A0"}, {"BlockL1-1A0", "1A00"}, {"BlockL1-1A0", "1A0Z"}, {"X", "XA0001"}} {
		if !edges[e] {
			t.Fatalf("edge %+v missing; got %+v", e, res.Edges)
		}
	}
	if len(res.Edges) != 4 || len(res.Skipped) != 0 {
		t.Fatalf("edges = %d skipped = %v, want 4 and none", len(res.Edges), res.Skipped)
	}
	if err := ValidateCounts(CountByClassKind(res), map[string]int{"chapter": 2, "block": 1, "category": 3}); err != nil {
		t.Fatalf("ValidateCounts: %v", err)
	}
	if err := ValidateCounts(CountByClassKind(res), map[string]int{"chapter": 28}); err == nil {
		t.Fatal("ValidateCounts must fail on a count mismatch")
	}
}

func TestParseICD11_RejectsBadInput(t *testing.T) {
	dup := strings.Join([]string{
		icd11Header,
		icd11Row(fChol, lin+"3", "1A00", "", `"Cholera"`, "category", "1", "False", "01", "True", "", ""),
		icd11Row(fChol+"0", lin+"30", "1A00", "", `"Cholera again"`, "category", "1", "False", "01", "True", "", ""),
	}, "\n")
	if _, err := ParseICD11(strings.NewReader(dup)); err == nil {
		t.Fatal("duplicate code must fail")
	}
	orphan := icd11Header + "\nnote before any entry"
	if _, err := ParseICD11(strings.NewReader(orphan)); err == nil {
		t.Fatal("continuation before the first entry must fail")
	}
}

func TestParseICD9CM(t *testing.T) {
	res, err := ParseICD9CM(strings.NewReader("0001 Therapeutic ultrasound of vessels of head and neck\r\n016  Excision of lesion of skull\n\n"))
	if err != nil {
		t.Fatalf("ParseICD9CM: %v", err)
	}
	if len(res.Concepts) != 2 || res.Concepts[0].Code != "00.01" || res.Concepts[1].Code != "01.6" {
		t.Fatalf("codes = %+v, want 00.01 and 01.6", res.Concepts)
	}
	if res.Concepts[1].Display != "Excision of lesion of skull" || !res.Concepts[0].Selectable {
		t.Fatalf("unexpected concept %+v", res.Concepts[1])
	}
	if _, err := ParseICD9CM(strings.NewReader("12AB Bad code\n")); err == nil {
		t.Fatal("non-digit code must fail")
	}
}

func TestVerifySHA256(t *testing.T) {
	// sha256("abc")
	if err := VerifySHA256([]byte("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"); err != nil {
		t.Fatalf("matching hash rejected: %v", err)
	}
	if err := VerifySHA256([]byte("abd"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"); err == nil {
		t.Fatal("mismatching hash accepted")
	}
}
