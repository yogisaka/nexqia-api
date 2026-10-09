package server

import (
	"reflect"
	"testing"
)

func TestCheckMergeIdentity(t *testing.T) {
	base := mergeIdentity{BirthDate: "1990-01-02", Gender: "female"}
	with := func(hash, typ string) mergeIdentity { m := base; m.IdentityHash, m.IdentityType = hash, typ; return m }
	other := mergeIdentity{BirthDate: "1991-01-02", Gender: "female"}
	for name, tc := range map[string]struct {
		s, m   mergeIdentity
		result string
		reason string
	}{
		"same hash":                   {with("h1", "ktp"), with("h1", "ktp"), "same_identity", ""},
		"different hash same type":    {with("h1", "ktp"), with("h2", "ktp"), "blocked", "identity_mismatch"},
		"different hash both untyped": {with("h1", ""), with("h2", ""), "blocked", "identity_mismatch"},
		"different types basics ok":   {with("h1", "paspor"), with("h2", "kitas"), "needs_documents", ""},
		"one identity basics ok":      {with("h1", "ktp"), base, "ok", ""},
		"one identity basics differ":  {with("h1", "ktp"), other, "blocked", "basics_mismatch"},
		"none basics ok":              {base, base, "needs_documents", ""},
		"none basics differ":          {base, other, "blocked", "basics_mismatch"},
		"none missing birth date":     {mergeIdentity{Gender: "female"}, mergeIdentity{Gender: "female"}, "blocked", "basics_mismatch"},
	} {
		got := checkMergeIdentity(tc.s, tc.m)
		if got.Result != tc.result || (tc.reason != "" && (len(got.Reasons) != 1 || got.Reasons[0] != tc.reason)) {
			t.Errorf("%s: got %+v, want %s %s", name, got, tc.result, tc.reason)
		}
	}
	sameHashDiffBasics := checkMergeIdentity(with("h1", "ktp"), mergeIdentity{IdentityHash: "h1", IdentityType: "ktp", BirthDate: "1980-01-01", Gender: "female"})
	if sameHashDiffBasics.Result != "same_identity" || len(sameHashDiffBasics.Warnings) != 1 {
		t.Errorf("same hash with different basics must warn, got %+v", sameHashDiffBasics)
	}
}

func TestResolveSurvivorship(t *testing.T) {
	s := map[string]string{"full_name": "Siti Aminah", "phone": "0812", "email": "", "address": "Lama", "family_id": "", "nik": "", "nik_search_hash": "", "identity_type_concept_id": ""}
	m := map[string]string{"full_name": "Siti Aminah S", "phone": "0857", "email": "s@x.id", "address": "Lama", "family_id": "SURV", "nik": "enc", "nik_search_hash": "h1", "identity_type_concept_id": "ktp"}

	_, _, missing := resolveSurvivorship(s, m, map[string]string{}, "SURV")
	if !reflect.DeepEqual(missing, []string{"full_name", "phone"}) {
		t.Fatalf("missing = %v, want [full_name phone]", missing)
	}

	final, applied, missing := resolveSurvivorship(s, m, map[string]string{"full_name": "survivor", "phone": "merged"}, "SURV")
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	want := map[string]string{"full_name": "survivor", "phone": "merged", "email": "autofill", "family_id": "autofill", "identity": "autofill"}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	if final["full_name"] != "Siti Aminah" || final["phone"] != "0857" || final["email"] != "s@x.id" || final["address"] != "Lama" {
		t.Fatalf("final = %v", final)
	}
	if final["nik"] != "enc" || final["nik_search_hash"] != "h1" || final["identity_type_concept_id"] != "ktp" {
		t.Fatalf("identity group must move together: %v", final)
	}
	if final["family_id"] != "" {
		t.Fatalf("family_id pointing at the survivor must become empty, got %q", final["family_id"])
	}
}
