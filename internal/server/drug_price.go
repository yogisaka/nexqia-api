package server

import (
	"errors"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"
)

// numericToMilli converts a numeric(12,3) value (packaging factor) to
// thousandths exactly; more than 3 non-zero fraction digits is an error.
func numericToMilli(n pgtype.Numeric) (int64, error) {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return 0, errors.New("numeric is not a finite value")
	}
	v := new(big.Int).Set(n.Int)
	exp := n.Exp + 3
	ten := big.NewInt(10)
	for ; exp > 0; exp-- {
		v.Mul(v, ten)
	}
	for ; exp < 0; exp++ {
		q, r := new(big.Int).QuoRem(v, ten, new(big.Int))
		if r.Sign() != 0 {
			return 0, errors.New("numeric has more than 3 fraction digits")
		}
		v = q
	}
	if !v.IsInt64() {
		return 0, errors.New("numeric out of range")
	}
	return v.Int64(), nil
}

// drugSellPriceCents is the base-unit sell price (spec §4.2.2): the package
// reference price divided by the package factor, plus the margin, rounded UP
// to a cent, then UP to a multiple of roundingCents (0 = none), capped at the
// HET (hetCents 0 = no HET). factorMilli > 0, marginBP > -10000.
func drugSellPriceCents(packCents, factorMilli, marginBP, roundingCents, hetCents int64) (int64, bool) {
	num := new(big.Int).Mul(big.NewInt(packCents), big.NewInt(10000+marginBP))
	num.Mul(num, big.NewInt(1000))
	den := new(big.Int).Mul(big.NewInt(factorMilli), big.NewInt(10000))
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	cents := q.Int64()
	if roundingCents > 0 && cents%roundingCents != 0 {
		cents = (cents/roundingCents + 1) * roundingCents
	}
	if hetCents > 0 && cents > hetCents {
		return hetCents, true
	}
	return cents, false
}
