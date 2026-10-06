package termimport

import (
	"strings"
	"testing"
)

func TestLoadProfiles(t *testing.T) {
	profiles, err := LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
	}
	for _, uri := range ProfileFiles {
		if _, ok := profiles[uri]; !ok {
			t.Fatalf("profile for %s missing", uri)
		}
	}
}

func TestValidateProfile_Rejects(t *testing.T) {
	valid := `{"as_of":"2026-10","status":"optional","recommended_for":["rumah_sakit"],
      "support":[{"target":"who","level":"native"},{"target":"bpjs_pcare","level":"none"},
        {"target":"bpjs_inacbg","level":"none"},{"target":"satusehat","level":"none"},{"target":"kemenkes_rl","level":"none"}],
      "pros":["a","b"],"cons":["c","d"],"sources":[{"label":"x","url":"https://x"}]}`
	if err := ValidateProfile([]byte(valid)); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"bad target":  strings.Replace(valid, `"target":"who"`, `"target":"whoo"`, 1),
		"bad level":   strings.Replace(valid, `"level":"native"`, `"level":"full"`, 1),
		"one pro":     strings.Replace(valid, `"pros":["a","b"]`, `"pros":["a"]`, 1),
		"long con":    strings.Replace(valid, `"c"`, `"`+strings.Repeat("x", 141)+`"`, 1),
		"bad status":  strings.Replace(valid, `"optional"`, `"required"`, 1),
		"unknown key": strings.Replace(valid, `"as_of"`, `"extra":1,"as_of"`, 1),
		"http source": strings.Replace(valid, `https://x`, `http://x`, 1),
		"dup target":  strings.Replace(valid, `"target":"bpjs_pcare"`, `"target":"who"`, 1),
	} {
		if err := ValidateProfile([]byte(bad)); err == nil {
			t.Fatalf("%s: invalid profile accepted", name)
		}
	}
}
