package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image/png"
	"strings"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

const visitTokenAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomTokenCode menghasilkan kode raw (prefix + 16 char Crockford base32, ~80 bit)
// dan sha256 hex-nya. Raw hanya dikembalikan sekali ke pemanggil (pola display_board.go:317-327).
func randomTokenCode(prefix string) (raw string, hash string, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	var sb strings.Builder
	sb.WriteString(prefix)
	for _, c := range b {
		sb.WriteByte(visitTokenAlphabet[int(c)%32])
	}
	raw = sb.String()
	return raw, hashTokenCode(raw), nil
}

func hashTokenCode(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// isValidTokenFormat: prefix + tepat 16 char dari alfabet (tanpa I/L/O/U).
func isValidTokenFormat(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) || len(s) != len(prefix)+16 {
		return false
	}
	for _, r := range s[len(prefix):] {
		if !strings.ContainsRune(visitTokenAlphabet, r) {
			return false
		}
	}
	return true
}

// QRPNGDataURI me-render payload jadi QR PNG data URI (encode M, 200x200) —
// pola internal/mfa QRDataURI (mfa.go:113) untuk payload arbitrary.
func QRPNGDataURI(payload string) (string, error) {
	code, err := qr.Encode(payload, qr.M, qr.Auto)
	if err != nil {
		return "", err
	}
	code, err = barcode.Scale(code, 200, 200)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, code); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// visitTokenExpiry = tengah malam lokal merchant hari berikutnya + 2 jam (spec §4.1).
func visitTokenExpiry(now time.Time, loc *time.Location) time.Time {
	return localDayStart(now.Add(24*time.Hour), loc).Add(2 * time.Hour)
}

// resolveVisitMedia: baris (merchant, department) → baris (merchant, NULL) → fallback "slip".
func resolveVisitMedia(rows []sqlcgen.OperationsVisitMediaConfig, departmentID pgtype.UUID) string {
	if departmentID.Valid {
		for _, r := range rows {
			if r.DepartmentID.Valid && r.DepartmentID.Bytes == departmentID.Bytes {
				return r.Media
			}
		}
	}
	for _, r := range rows {
		if !r.DepartmentID.Valid {
			return r.Media
		}
	}
	return "slip"
}
