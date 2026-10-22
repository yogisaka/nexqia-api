package server

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// TestVisitTokenCodeFormat: prefix + 16 char Crockford base32, hash = sha256 hex 64 char.
func TestVisitTokenCodeFormat(t *testing.T) {
	for _, prefix := range []string{"NQV1-", "NQP1-"} {
		raw, hash, err := randomTokenCode(prefix)
		if err != nil {
			t.Fatalf("randomTokenCode(%q): %v", prefix, err)
		}
		if !strings.HasPrefix(raw, prefix) {
			t.Errorf("raw %q missing prefix %q", raw, prefix)
		}
		if len(raw) != len(prefix)+16 {
			t.Errorf("raw %q length = %d, want %d", raw, len(raw), len(prefix)+16)
		}
		for _, r := range raw[len(prefix):] {
			if !strings.ContainsRune(visitTokenAlphabet, r) {
				t.Errorf("raw %q has invalid char %q", raw, string(r))
			}
		}
		if len(hash) != 64 {
			t.Errorf("hash %q length = %d, want 64", hash, len(hash))
		}
		for _, r := range hash {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Errorf("hash %q has non-hex char %q", hash, string(r))
			}
		}
		if hash != hashTokenCode(raw) {
			t.Errorf("hash %q != sha256(%q)", hash, raw)
		}
	}
	raw1, _, _ := randomTokenCode("NQV1-")
	raw2, _, _ := randomTokenCode("NQV1-")
	if raw1 == raw2 {
		t.Errorf("two calls produced identical code %q", raw1)
	}
}

// TestVisitTokenCodeInvalidFormat: isValidTokenFormat menolak bentuk salah.
func TestVisitTokenCodeInvalidFormat(t *testing.T) {
	valid := "NQV1-0123456789ABCDEF" // prefix + 16 char alfabet
	if !isValidTokenFormat(valid, "NQV1-") {
		t.Fatalf("isValidTokenFormat rejected valid %q", valid)
	}
	cases := []string{
		"0123456789ABCDEF",       // tanpa prefix
		"NQV1-0123456789ABCDE",   // 15 char (panjang salah)
		"NQV1-0123456789ABCDEF0", // 17 char (panjang salah)
		"NQV1-I123456789ABCDEF",  // char I tidak di alfabet
		"NQV1-L123456789ABCDEF",  // char L tidak di alfabet
		"NQV1-O123456789ABCDEF",  // char O tidak di alfabet
		"NQV1-U123456789ABCDEF",  // char U tidak di alfabet
		"NQV1-a123456789ABCDEF",  // huruf kecil ditolak
	}
	for _, s := range cases {
		if isValidTokenFormat(s, "NQV1-") {
			t.Errorf("isValidTokenFormat accepted invalid %q", s)
		}
	}
}

// TestQRPNGDataURI: data URI PNG dari payload.
func TestQRPNGDataURI(t *testing.T) {
	uri, err := QRPNGDataURI("https://example.test/v/NQV1-0123456789ABCDEF")
	if err != nil {
		t.Fatalf("QRPNGDataURI: %v", err)
	}
	const p = "data:image/png;base64,"
	if !strings.HasPrefix(uri, p) {
		t.Fatalf("uri %q missing prefix %q", uri[:40], p)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, p))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if len(b) < 4 || string(b[:4]) != "\x89PNG" {
		t.Errorf("decoded payload is not PNG (first bytes %v)", b[:min(4, len(b))])
	}
}

// TestVisitTokenExpiryUsesMerchantZone: expiry = localDayStart(now+24h, loc) + 2 jam.
func TestVisitTokenExpiryUsesMerchantZone(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-20T15:00:00+07:00")
	jkt, _ := time.LoadLocation("Asia/Jakarta")
	want, _ := time.Parse(time.RFC3339, "2026-10-21T02:00:00+07:00")
	if got := visitTokenExpiry(now, jkt); !got.Equal(want) {
		t.Errorf("Jakarta expiry = %v, want %v", got, want)
	}
	wantUTC, _ := time.Parse(time.RFC3339, "2026-10-21T02:00:00Z")
	if got := visitTokenExpiry(now, time.UTC); !got.Equal(wantUTC) {
		t.Errorf("UTC expiry = %v, want %v", got, wantUTC)
	}
}
