// Package termimport parses official terminology releases and loads them
// into terminology.* — spec 2026-10-01-terminology-import-design §4.
package termimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Release pins one downloadable release: the archive URL, its SHA256, the
// member file(s) to read, and (ICD-11) the expected row counts per ClassKind.
type Release struct {
	URL    string
	SHA256 string
	Files  []string
	Counts map[string]int
}

// Manifest is pinned in code (not env): dataset → release → Release.
var Manifest = map[string]map[string]Release{
	"icd11": {"2026-01": {
		URL:    "https://icdcdn.who.int/static/releasefiles/2026-01/SimpleTabulation-ICD-11-MMS-en.zip",
		SHA256: "f1356588f40953a83e3af2b662deab47c5e269f944d1ea4ed0cfeb2007c7cd39",
		Files:  []string{"SimpleTabulation-ICD-11-MMS-en.txt"},
		Counts: map[string]int{"chapter": 28, "block": 1360, "category": 35664},
	}},
	"icd9cm": {"v32": {
		URL:    "https://www.cms.gov/medicare/coding/icd9providerdiagnosticcodes/downloads/icd-9-cm-v32-master-descriptions.zip",
		SHA256: "45a7d05ddcadf124af88375b64cdf068bb1e3f999ce7fdacb91f65f4e6d55f08",
		Files:  []string{"CMS32_DESC_LONG_SG.txt"},
	}},
	"icd10-icd11-map": {"2026-01": {
		URL:    "https://icdcdn.who.int/static/releasefiles/2026-01/mapping.zip",
		SHA256: "2eb158cf2a0d53690d6e9baf0956f7617e1315e4f2a44dfc3db3be8f49193a1d",
		Files:  []string{"10To11MapToMultipleCategories.txt", "10To11MapToOneCategory.txt", "11To10MapToOneCategory.txt"},
	}},
}

// DefaultRelease is the release used when --release is not given.
var DefaultRelease = map[string]string{"icd11": "2026-01", "icd9cm": "v32", "icd10-icd11-map": "2026-01"}

// VerifySHA256 fails when data does not hash to want (hex, case-insensitive).
func VerifySHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
