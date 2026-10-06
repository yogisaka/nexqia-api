package termimport

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"
)

//go:embed profiles/*.json
var profileFS embed.FS

// ProfileFiles maps each embedded profile to the code_system it describes.
var ProfileFiles = map[string]string{
	"icd-10.json":   "http://hl7.org/fhir/sid/icd-10",
	"icd-11.json":   "http://id.who.int/icd/release/11/mms",
	"icd-9-cm.json": "http://hl7.org/fhir/sid/icd-9-cm",
}

type profileSupport struct {
	Target string `json:"target"`
	Level  string `json:"level"`
	Note   string `json:"note,omitempty"`
}

type profileSource struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type profile struct {
	AsOf           string           `json:"as_of"`
	Status         string           `json:"status"`
	RecommendedFor []string         `json:"recommended_for"`
	Support        []profileSupport `json:"support"`
	Pros           []string         `json:"pros"`
	Cons           []string         `json:"cons"`
	Warning        string           `json:"warning,omitempty"`
	Sources        []profileSource  `json:"sources"`
}

var validStatuses = map[string]bool{
	"mandatory":    true,
	"optional":     true,
	"future_ready": true,
}

var validRecommendations = map[string]bool{
	"klinik_pratama": true,
	"klinik_utama":   true,
	"rumah_sakit":    true,
}

var validTargets = map[string]bool{
	"who":         true,
	"bpjs_pcare":  true,
	"bpjs_inacbg": true,
	"satusehat":   true,
	"kemenkes_rl": true,
}

var validLevels = map[string]bool{
	"native":      true,
	"via_mapping": true,
	"none":        true,
}

// ValidateProfile checks that raw is a well-formed profile document.
func ValidateProfile(raw []byte) error {
	var p profile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	if p.AsOf == "" {
		return fmt.Errorf("profile: as_of must not be empty")
	}
	if !validStatuses[p.Status] {
		return fmt.Errorf("profile: status %q is invalid", p.Status)
	}
	for _, r := range p.RecommendedFor {
		if !validRecommendations[r] {
			return fmt.Errorf("profile: recommended_for %q is invalid", r)
		}
	}
	if len(p.Support) != 5 {
		return fmt.Errorf("profile: support must have 5 entries, got %d", len(p.Support))
	}
	seen := map[string]bool{}
	for _, s := range p.Support {
		if !validTargets[s.Target] {
			return fmt.Errorf("profile: support target %q is invalid", s.Target)
		}
		if seen[s.Target] {
			return fmt.Errorf("profile: support target %q is duplicated", s.Target)
		}
		seen[s.Target] = true
		if !validLevels[s.Level] {
			return fmt.Errorf("profile: support level %q is invalid", s.Level)
		}
	}
	if err := validateItems("pros", p.Pros); err != nil {
		return err
	}
	if err := validateItems("cons", p.Cons); err != nil {
		return err
	}
	if len(p.Sources) < 1 {
		return fmt.Errorf("profile: sources must have at least 1 entry")
	}
	for _, s := range p.Sources {
		if s.Label == "" {
			return fmt.Errorf("profile: source label must not be empty")
		}
		if s.URL == "" {
			return fmt.Errorf("profile: source url must not be empty")
		}
		if len(s.URL) < 8 || s.URL[:8] != "https://" {
			return fmt.Errorf("profile: source url %q must start with https://", s.URL)
		}
	}
	return nil
}

func validateItems(name string, items []string) error {
	if len(items) < 2 || len(items) > 4 {
		return fmt.Errorf("profile: %s must have 2-4 items, got %d", name, len(items))
	}
	for i, it := range items {
		n := utf8.RuneCountInString(it)
		if n < 1 || n > 140 {
			return fmt.Errorf("profile: %s[%d] length %d, must be 1-140 characters", name, i, n)
		}
	}
	return nil
}

// LoadProfiles reads, validates, and returns all embedded profiles keyed by system_uri.
func LoadProfiles() (map[string][]byte, error) {
	matches, err := fs.Glob(profileFS, "profiles/*.json")
	if err != nil {
		return nil, fmt.Errorf("profile: enumerate embedded profiles: %w", err)
	}
	out := make(map[string][]byte, len(ProfileFiles))
	for _, path := range matches {
		file := strings.TrimPrefix(path, "profiles/")
		uri, ok := ProfileFiles[file]
		if !ok {
			return nil, fmt.Errorf("profile %s: embedded file is not registered in ProfileFiles", file)
		}
		raw, err := fs.ReadFile(profileFS, path)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", file, err)
		}
		if err := ValidateProfile(raw); err != nil {
			return nil, fmt.Errorf("profile %s: %w", file, err)
		}
		out[uri] = raw
	}
	read := make(map[string]bool, len(matches))
	for _, path := range matches {
		read[strings.TrimPrefix(path, "profiles/")] = true
	}
	for file := range ProfileFiles {
		if !read[file] {
			return nil, fmt.Errorf("profile %s: registered in ProfileFiles but not embedded", file)
		}
	}
	if len(out) != len(ProfileFiles) {
		return nil, fmt.Errorf("profile: got %d profiles, want %d", len(out), len(ProfileFiles))
	}
	return out, nil
}
