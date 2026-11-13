package main

import (
	"strconv"
	"strings"
	"testing"
)

const validFixture = `indicator,gender,x_kind,x,l,m,s
bbu,laki_laki,age_days,1.00,-0.3521,3.3483,0.14570
bbu,laki_laki,age_days,548.00,-0.2165,8.9570,0.11230
bbtb,perempuan,height_cm,65.50,-0.3833,7.2470,0.08920
`

func TestParseCSVAcceptsFixture(t *testing.T) {
	rows, err := parseCSV(strings.NewReader(validFixture), "bbu_laki_laki.csv")
	if err != nil {
		t.Fatalf("parseCSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("dapat %d baris, mau 3", len(rows))
	}
	// x=548.00 (WHO expanded table day 548) must survive as-is.
	if rows[1].X != "548.00" {
		t.Errorf("baris 2 x = %q, mau 548.00", rows[1].X)
	}
	if rows[0].Indicator != "bbu" || rows[0].Gender != "laki_laki" || rows[0].XKind != "age_days" {
		t.Errorf("baris 1 metadata salah: %+v", rows[0])
	}
	if rows[2].XKind != "height_cm" || rows[2].Gender != "perempuan" {
		t.Errorf("baris 3 metadata salah: %+v", rows[2])
	}
}

func TestParseCSVRejectsBadHeader(t *testing.T) {
	_, err := parseCSV(strings.NewReader("indicator,gender,x,l,m,s\n"), "x.csv")
	if err == nil || !strings.Contains(err.Error(), "x.csv") || !strings.Contains(err.Error(), "baris 1") {
		t.Fatalf("mau error nama file+baris, dapat %v", err)
	}
}

func TestParseCSVRejectsUnknownIndicator(t *testing.T) {
	csv := "indicator,gender,x_kind,x,l,m,s\nfoo,laki_laki,age_days,1.00,1,1,1\n"
	_, err := parseCSV(strings.NewReader(csv), "x.csv")
	if err == nil || !strings.Contains(err.Error(), "indicator \"foo\"") || !strings.Contains(err.Error(), "baris 2") {
		t.Fatalf("mau error indicator foo baris 2, dapat %v", err)
	}
}

func TestParseCSVRejectsZeroS(t *testing.T) {
	csv := "indicator,gender,x_kind,x,l,m,s\nbbu,perempuan,age_days,1.00,1,1,0\n"
	_, err := parseCSV(strings.NewReader(csv), "x.csv")
	if err == nil || !strings.Contains(err.Error(), "s harus > 0") {
		t.Fatalf("mau error s=0, dapat %v", err)
	}
}

func TestParseCSVRejectsUnknownGender(t *testing.T) {
	csv := "indicator,gender,x_kind,x,l,m,s\nbbu,cowboy,age_days,1.00,1,1,1\n"
	_, err := parseCSV(strings.NewReader(csv), "x.csv")
	if err == nil || !strings.Contains(err.Error(), "gender \"cowboy\"") {
		t.Fatalf("mau error gender cowboy, dapat %v", err)
	}
}

func TestParseCSVRejectsNonNumericX(t *testing.T) {
	csv := "indicator,gender,x_kind,x,l,m,s\nbbu,perempuan,age_days,abc,1,1,1\n"
	_, err := parseCSV(strings.NewReader(csv), "x.csv")
	if err == nil || !strings.Contains(err.Error(), "kolom x") {
		t.Fatalf("mau error kolom x, dapat %v", err)
	}
}

// TestInsertSQLParameterOrder pins the builder: the VALUES clause carries
// exactly $1..$11, and the column list follows the contract order so that the
// Exec argument list (indicator, gender, x_kind, x, l, m, s, source_name,
// source_version, source_url, downloaded_at) lines up 1:1. Conflict target =
// natural key (indicator, gender, x, source_version).
func TestInsertSQLParameterOrder(t *testing.T) {
	cols := []string{"indicator", "gender", "x_kind", "x", "l", "m", "s", "source_name", "source_version", "source_url", "downloaded_at"}
	for i, c := range cols {
		ph := "$" + strconv.Itoa(i+1)
		if !strings.Contains(insertSQL, ph) {
			t.Fatalf("insertSQL tidak memuat placeholder %s (kolom %s)", ph, c)
		}
	}
	if strings.Contains(insertSQL, "$12") {
		t.Errorf("insertSQL tidak boleh memuat $12:\n%s", insertSQL)
	}
	last := -1
	for _, c := range cols {
		pos := strings.Index(insertSQL, c)
		if pos < 0 {
			t.Fatalf("insertSQL tidak memuat kolom %s", c)
		}
		if pos < last {
			t.Errorf("kolom %s tidak urut sesuai kontrak", c)
		}
		last = pos
	}
	if !strings.Contains(insertSQL, "ON CONFLICT (indicator, gender, x, source_version) DO NOTHING") {
		t.Errorf("insertSQL harus ON CONFLICT (indicator, gender, x, source_version) DO NOTHING, dapat:\n%s", insertSQL)
	}
}

// TestSourceURLCoversIndicators pins the literal per-indicator map (bbtb →
// weight-for-height; sisanya → standards/<indikator>).
func TestSourceURLCoversIndicators(t *testing.T) {
	for ind := range validIndicators {
		if _, ok := sourceURL[ind]; !ok {
			t.Errorf("sourceURL tidak memuat %s", ind)
		}
	}
	if got := sourceURL["bbtb"]; !strings.HasSuffix(got, "/weight-for-height") {
		t.Errorf("bbtb source_url = %q, mau berakhiran /weight-for-height", got)
	}
}
