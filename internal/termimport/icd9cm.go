package termimport

import (
	"fmt"
	"io"
	"strings"
)

// ParseICD9CM parses the CMS ICD-9-CM v32 master description format (§4.2):
// one description per line, code then space(s) then title.
func ParseICD9CM(r io.Reader) (ParseResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return ParseResult{}, err
	}
	res := ParseResult{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		code, rest, _ := strings.Cut(line, " ")
		title := strings.TrimSpace(rest)
		if (len(code) != 3 && len(code) != 4) || strings.Trim(code, "0123456789") != "" {
			return ParseResult{}, fmt.Errorf("bad ICD-9-CM code %q", code)
		}
		code = code[:2] + "." + code[2:]
		if seen[code] {
			return ParseResult{}, fmt.Errorf("duplicate ICD-9-CM code %s", code)
		}
		seen[code] = true
		res.Concepts = append(res.Concepts, Concept{
			Code:       code,
			Display:    title,
			Selectable: true,
			Properties: map[string]any{},
		})
	}
	return res, nil
}
