// internal/auth/token.go
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims identifies the authenticated principal: which app_user, in which company,
// with which merchant active for this session (see internal/session — chosen at
// login or via /auth/switch-merchant, see docs/design/specs/2026-09-15-session-refresh-token-design.md §3a).
// MerchantID here is informational (session identity, shown in UI) — it is NOT the
// source of authorization; server.RequirePermission independently checks X-Merchant-ID
// against user_merchant_role on every request, unchanged by this field.
type Claims struct {
	UserID     string `json:"uid"`
	CompanyID  string `json:"cid"`
	MerchantID string `json:"mid"`
	Username   string `json:"username"`
	jwt.RegisteredClaims
}

// GenerateToken issues a short-lived access token. ttl comes from
// config.Config.AccessTokenTTLMinutes — see docs/design/specs/2026-09-15-session-refresh-token-design.md.
func GenerateToken(secret, userID, companyID, merchantID, username string, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:     userID,
		CompanyID:  companyID,
		MerchantID: merchantID,
		Username:   username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// merchantSelectionPurpose marks a merchant-selection token so it can never be
// accepted by AuthMiddleware/ParseToken as a real access token by mistake.
const merchantSelectionPurpose = "merchant_selection"

// MerchantSelectionClaims is issued after password verification when a user
// belongs to more than one merchant (spec §3a) — carries only the user's identity,
// not company/merchant, and is only valid at POST /auth/select-merchant.
type MerchantSelectionClaims struct {
	UserID  string `json:"uid"`
	Purpose string `json:"purpose"`
	jwt.RegisteredClaims
}

func GenerateMerchantSelectionToken(secret, userID string, ttl time.Duration) (string, error) {
	claims := MerchantSelectionClaims{
		UserID:  userID,
		Purpose: merchantSelectionPurpose,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ErrNotMerchantSelectionToken means a token parsed successfully but wasn't
// issued by GenerateMerchantSelectionToken (wrong purpose claim).
var ErrNotMerchantSelectionToken = errors.New("not a merchant-selection token")

func ParseMerchantSelectionToken(secret, tokenString string) (*MerchantSelectionClaims, error) {
	claims := &MerchantSelectionClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	if claims.Purpose != merchantSelectionPurpose {
		return nil, ErrNotMerchantSelectionToken
	}
	return claims, nil
}

func ParseToken(secret, tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}
