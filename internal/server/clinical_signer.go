// internal/server/clinical_signer.go
// TTE internal for clinical form entries (spec
// 2026-10-21-cppt-rajal-fase1-design §6.2, STD-04): deterministic SHA-256
// entry signature. The package var entrySigner lets a PSrE implementation
// replace the hash signer later without touching the schema (STD-04).
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// EntrySigner produces the immutable signature hash stored on a signed
// clinical form entry (spec §6.2).
type EntrySigner interface {
	Signature(entryID, admissionID, formType string, templateVersion int, filledBy string, filledAt time.Time, formData map[string]any) (string, error)
}

// entrySigner is the active signer implementation (STD-04: swap for PSrE
// without changing the form_instance columns).
var entrySigner EntrySigner = sha256EntrySigner{}

// sha256EntrySigner hashes the signature preimage
// "nexqia-clinical-sign-v1|entry|admission|formType|version|filledBy|filledAt|canonicalFormData"
// with SHA-256; json.Marshal of a map emits keys in sorted order (including
// nested maps), so the canonical form is deterministic (spec §6.2 LITERAL).
type sha256EntrySigner struct{}

func (sha256EntrySigner) Signature(entryID, admissionID, formType string, templateVersion int, filledBy string, filledAt time.Time, formData map[string]any) (string, error) {
	canonicalFormData, err := json.Marshal(formData)
	if err != nil {
		return "", err
	}
	preimage := fmt.Sprintf("nexqia-clinical-sign-v1|%s|%s|%s|%d|%s|%s|%s",
		entryID, admissionID, formType, templateVersion, filledBy,
		filledAt.Format(time.RFC3339Nano), string(canonicalFormData))
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:]), nil
}

// VerifyEntrySignature recomputes the signature from the stored entry fields
// and compares it with the stored hash (spec §6.2). Counter-sign/audit fields
// are not part of the preimage, so a counter-sign never breaks verification.
func VerifyEntrySignature(entry sqlcgen.ClinicalFormFormInstance) error {
	if !entry.SignatureHash.Valid {
		return errors.New("signature mismatch")
	}
	var formData map[string]any
	if err := json.Unmarshal(entry.FormData, &formData); err != nil {
		return err
	}
	hash, err := entrySigner.Signature(
		entry.ID.String(), entry.AdmissionID.String(), entry.FormType,
		int(entry.TemplateVersion), entry.FilledBy.String(), entry.FilledAt.Time, formData)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(entry.SignatureHash.String)) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}
