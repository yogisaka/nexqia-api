// internal/server/money.go
// Exact money handling for tariffs (spec 2026-10-07-tariff-price-lists §4):
// amounts are integer cents; JSON carries decimal strings with 2 digits.
package server

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

var errInvalidAmount = errors.New("invalid amount, expected a non-negative decimal with at most 2 fraction digits")

// parseCents parses "75000", "75000.5" or "75000.50" into cents. Negative
// values, more than 2 fraction digits, signs, exponents and blanks fail.
func parseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errInvalidAmount
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 || (hasDot && frac == "") {
		return 0, errInvalidAmount
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, errInvalidAmount
		}
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if len(whole) > 13 {
		return 0, errInvalidAmount
	}
	// Base 10 explicitly: "0.80" must not be read as octal "080".
	cents, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, errInvalidAmount
	}
	return cents, nil
}

// formatCents renders cents as "75000.00".
func formatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// numericToCents converts a numeric(15,2)/(7,2) value exactly; more than 2
// fraction digits that are non-zero is an error.
func numericToCents(n pgtype.Numeric) (int64, error) {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return 0, errors.New("numeric is not a finite value")
	}
	v := new(big.Int).Set(n.Int)
	exp := n.Exp + 2
	ten := big.NewInt(10)
	for ; exp > 0; exp-- {
		v.Mul(v, ten)
	}
	for ; exp < 0; exp++ {
		q, r := new(big.Int).QuoRem(v, ten, new(big.Int))
		if r.Sign() != 0 {
			return 0, errors.New("numeric has more than 2 fraction digits")
		}
		v = q
	}
	if !v.IsInt64() {
		return 0, errors.New("numeric out of range")
	}
	return v.Int64(), nil
}

// centsToNumeric builds an exact numeric with 2 fraction digits.
func centsToNumeric(c int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(c), Exp: -2, Valid: true}
}
