package termimport

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ExpectedWilayah holds the official Kemendagri region counts per level
// (total 91,599 rows).
var ExpectedWilayah = struct {
	Provinsi  int
	KabKota   int
	Kecamatan int
	DesaKel   int
}{Provinsi: 38, KabKota: 514, Kecamatan: 7285, DesaKel: 83762}

// wilayahTupleRe extracts one ('<code>','<name>') tuple from the SQL dump.
var wilayahTupleRe = regexp.MustCompile(`\('([0-9]+(?:\.[0-9]+){0,3})','((?:[^']|'')*)'\)`)

// isoAlpha2Re validates ISO 3166-1 alpha-2 codes.
var isoAlpha2Re = regexp.MustCompile(`^[A-Z]{2}$`)

// ParseWilayahSQL parses the Kemendagri region SQL dump (§). Codes are
// dot-separated: 1 segment = provinsi, 2 = kab/kota, 3 = kecamatan,
// 4 = desa/kelurahan; segments 1–3 have exactly 2 digits, segment 4 exactly 4.
func ParseWilayahSQL(r io.Reader) (ParseResult, error) {
	want := [4]int{ExpectedWilayah.Provinsi, ExpectedWilayah.KabKota, ExpectedWilayah.Kecamatan, ExpectedWilayah.DesaKel}
	return parseWilayahCounts(r, want)
}

// parseWilayahCounts is ParseWilayahSQL with injected per-level expectations
// (provinsi, kab/kota, kecamatan, desa/kelurahan).
func parseWilayahCounts(r io.Reader, want [4]int) (ParseResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return ParseResult{}, err
	}
	matches := wilayahTupleRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return ParseResult{}, fmt.Errorf("wilayah: 0 tuple ditemukan")
	}

	var counts [4]int
	res := ParseResult{Concepts: make([]Concept, 0, len(matches))}
	seen := map[string]bool{}
	for _, m := range matches {
		code := m[1]
		display := strings.ReplaceAll(m[2], "''", "'")
		segs := strings.Split(code, ".")
		switch len(segs) {
		case 1:
			if !matchDigits(segs[0], 2) {
				return ParseResult{}, fmt.Errorf("wilayah: struktur segmen salah untuk kode %s", code)
			}
		case 2, 3:
			for _, s := range segs {
				if !matchDigits(s, 2) {
					return ParseResult{}, fmt.Errorf("wilayah: struktur segmen salah untuk kode %s", code)
				}
			}
		case 4:
			for i, s := range segs {
				want := 2
				if i == 3 {
					want = 4
				}
				if !matchDigits(s, want) {
					return ParseResult{}, fmt.Errorf("wilayah: struktur segmen salah untuk kode %s", code)
				}
			}
		default:
			return ParseResult{}, fmt.Errorf("wilayah: struktur segmen salah untuk kode %s", code)
		}
		if display == "" {
			return ParseResult{}, fmt.Errorf("wilayah: display kosong untuk kode %s", code)
		}
		if seen[code] {
			return ParseResult{}, fmt.Errorf("wilayah: kode duplikat %s", code)
		}
		seen[code] = true
		counts[len(segs)-1]++
		res.Concepts = append(res.Concepts, Concept{Code: code, Display: display, Selectable: true})
		if len(segs) > 1 {
			parent := strings.Join(segs[:len(segs)-1], ".")
			if !seen[parent] {
				return ParseResult{}, fmt.Errorf("wilayah: parent %s tidak ditemukan untuk kode %s", parent, code)
			}
			res.Edges = append(res.Edges, Edge{Parent: parent, Child: code})
		}
	}

	labels := [4]string{"provinsi", "kab/kota", "kecamatan", "desa/kelurahan"}
	for i := range want {
		if counts[i] != want[i] {
			return ParseResult{}, fmt.Errorf("wilayah: jumlah level %s: aktual %d, ekspektasi %d", labels[i], counts[i], want[i])
		}
	}

	kab, kota := countKabKota(res.Concepts)
	res.Skipped = []string{fmt.Sprintf("kab/kota: %d KAB / %d KOTA (dari prefix nama, tidak divalidasi)", kab, kota)}
	return res, nil
}

// matchDigits reports whether s consists of exactly n digits.
func matchDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// countKabKota splits kab/kota level rows by their name prefix ("KABUPATEN "
// vs "KOTA"); naming is not validated.
func countKabKota(concepts []Concept) (kab, kota int) {
	for _, c := range concepts {
		if strings.Count(c.Code, ".") != 1 {
			continue
		}
		if strings.HasPrefix(c.Display, "KOTA") {
			kota++
		} else {
			kab++
		}
	}
	return kab, kota
}

// isoEntry is one object of the ISO 3166-1 JSON list.
type isoEntry struct {
	Name   string `json:"name"`
	Alpha2 string `json:"alpha-2"`
	Alpha3 string `json:"alpha-3"`
}

// ParseISO3166JSON parses the ISO 3166-1 country list JSON.
func ParseISO3166JSON(r io.Reader) (ParseResult, error) {
	var entries []isoEntry
	if err := json.NewDecoder(r).Decode(&entries); err != nil {
		return ParseResult{}, fmt.Errorf("iso3166: decode gagal: %w", err)
	}
	if len(entries) != 249 {
		return ParseResult{}, fmt.Errorf("iso3166: jumlah entri %d, ekspektasi 249", len(entries))
	}
	seen := map[string]bool{}
	res := ParseResult{Concepts: make([]Concept, 0, len(entries))}
	for _, e := range entries {
		if !isoAlpha2Re.MatchString(e.Alpha2) {
			return ParseResult{}, fmt.Errorf("iso3166: alpha-2 tidak valid %q", e.Alpha2)
		}
		if seen[e.Alpha2] {
			return ParseResult{}, fmt.Errorf("iso3166: alpha-2 duplikat %s", e.Alpha2)
		}
		if e.Name == "" {
			return ParseResult{}, fmt.Errorf("iso3166: nama kosong untuk %s", e.Alpha2)
		}
		if e.Alpha2 == "ID" && e.Name != "Indonesia" {
			return ParseResult{}, fmt.Errorf("iso3166: entri ID harus bernama Indonesia, dapat %q", e.Name)
		}
		seen[e.Alpha2] = true
		res.Concepts = append(res.Concepts, Concept{Code: e.Alpha2, Display: e.Name, Selectable: true})
	}
	return res, nil
}
