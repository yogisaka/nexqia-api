package termimport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Concept is one row to upsert into terminology.concept.
type Concept struct {
	Code       string
	Display    string
	Selectable bool
	Properties map[string]any
}

// Edge is one parent→child is-a link between two concept codes.
type Edge struct {
	Parent string
	Child  string
}

// ParseResult is a parsed release; Skipped lists rows/edges not loaded, with reasons.
type ParseResult struct {
	Concepts []Concept
	Edges    []Edge
	Skipped  []string
}

// icd11Columns are the header column names ParseICD11 needs, looked up by name.
var icd11Columns = []string{
	"Foundation URI", "Linearization URI", "Code", "BlockId", "Title", "ClassKind",
	"DepthInKind", "IsResidual", "ChapterNo", "isLeaf", "CodingNote", "Parent",
}

// icd11EntryStart marks the Linearization URI column of a new entry line.
const icd11EntryStart = "http://id.who.int/icd/release/11/"

// icd11Entry holds one raw multi-line entry's fields.
type icd11Entry struct {
	found, lin, code, block, title, kind, depth, residual, chapter, leaf, note, parent string
}

// ParseICD11 parses the SimpleTabulation ICD-11 MMS release format (§4.1).
func ParseICD11(r io.Reader) (ParseResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return ParseResult{}, err
	}
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) == 0 {
		return ParseResult{}, errors.New("empty ICD-11 input")
	}

	header := strings.Split(lines[0], "\t")
	idx := map[string]int{}
	for i, name := range header {
		idx[name] = i
	}
	for _, name := range icd11Columns {
		if _, ok := idx[name]; !ok {
			return ParseResult{}, fmt.Errorf("header column %q not found", name)
		}
	}

	var raw []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) >= 2 && strings.HasPrefix(cols[1], icd11EntryStart) {
			raw = append(raw, line)
		} else {
			if len(raw) == 0 {
				return ParseResult{}, errors.New("continuation line before first entry")
			}
			raw[len(raw)-1] += "\n" + line
		}
	}

	wantCols := len(header) - 1 // data rows have no Version: column
	entries := make([]icd11Entry, 0, len(raw))
	seen := map[string]bool{}
	for i, e := range raw {
		cols := strings.Split(e, "\t")
		if len(cols) != wantCols {
			return ParseResult{}, fmt.Errorf("entry %d: %d columns, want %d", i+1, len(cols), wantCols)
		}
		en := icd11Entry{
			found:    cols[idx["Foundation URI"]],
			lin:      cols[idx["Linearization URI"]],
			code:     cols[idx["Code"]],
			block:    cols[idx["BlockId"]],
			title:    cols[idx["Title"]],
			kind:     cols[idx["ClassKind"]],
			depth:    cols[idx["DepthInKind"]],
			residual: cols[idx["IsResidual"]],
			chapter:  cols[idx["ChapterNo"]],
			leaf:     cols[idx["isLeaf"]],
			note:     cols[idx["CodingNote"]],
			parent:   cols[idx["Parent"]],
		}
		en.title = strings.TrimPrefix(en.title, `"`)
		en.title = strings.TrimSuffix(en.title, `"`)
		for strings.HasPrefix(en.title, "- ") {
			en.title = strings.TrimPrefix(en.title, "- ")
		}
		if en.title == "" {
			return ParseResult{}, fmt.Errorf("entry %d: empty title", i+1)
		}
		if en.lin == "" {
			return ParseResult{}, fmt.Errorf("entry %d: empty linearization URI", i+1)
		}
		switch en.kind {
		case "chapter":
			en.code = en.chapter
		case "block":
			en.code = en.block
		case "category":
			// en.code stays
		default:
			return ParseResult{}, fmt.Errorf("entry %d: unknown ClassKind %q", i+1, en.kind)
		}
		if en.code == "" {
			return ParseResult{}, fmt.Errorf("entry %d: empty code", i+1)
		}
		if seen[en.code] {
			return ParseResult{}, fmt.Errorf("entry %d: duplicate code %s", i+1, en.code)
		}
		seen[en.code] = true
		entries = append(entries, en)
	}

	res := ParseResult{Concepts: make([]Concept, 0, len(entries))}
	codeByFound := map[string]string{}
	for _, en := range entries {
		if en.found != "" {
			codeByFound[en.found] = en.code
		}
	}
	for _, en := range entries {
		depth, err := strconv.Atoi(en.depth)
		if err != nil {
			return ParseResult{}, fmt.Errorf("entry %s: bad DepthInKind %q", en.code, en.depth)
		}
		props := map[string]any{
			"class_kind":        en.kind,
			"chapter":           en.chapter,
			"depth_in_kind":     depth,
			"is_residual":       en.residual == "True",
			"is_leaf":           en.leaf == "True",
			"foundation_uri":    en.found,
			"linearization_uri": en.lin,
			"is_extension":      en.chapter == "X",
		}
		if en.note != "" {
			props["coding_note"] = en.note
		}
		res.Concepts = append(res.Concepts, Concept{
			Code:       en.code,
			Display:    en.title,
			Selectable: en.kind == "category" && en.chapter != "X",
			Properties: props,
		})
	}
	for _, en := range entries {
		if en.parent == "" {
			continue
		}
		parentCode, ok := codeByFound[en.parent]
		if !ok {
			res.Skipped = append(res.Skipped, fmt.Sprintf("parent %s not found for %s", en.parent, en.code))
			continue
		}
		res.Edges = append(res.Edges, Edge{Parent: parentCode, Child: en.code})
	}
	return res, nil
}

// CountByClassKind counts concepts per Properties["class_kind"].
func CountByClassKind(res ParseResult) map[string]int {
	counts := map[string]int{}
	for _, c := range res.Concepts {
		kind, _ := c.Properties["class_kind"].(string)
		counts[kind]++
	}
	return counts
}

// ValidateCounts fails when any wanted count does not match exactly.
func ValidateCounts(got, want map[string]int) error {
	for k, w := range want {
		g := got[k]
		if g != w {
			return fmt.Errorf("class kind %s: got %d, want %d", k, g, w)
		}
	}
	return nil
}
