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
// accessTokenPurpose marks a real access token so it can never be accepted
// where a narrow-purpose token (e.g. merchant-selection) is expected, or vice
// versa — tokens are positively typed, not distinguished by which fields
// happen to be empty (see MerchantSelectionClaims below).
const accessTokenPurpose = "access"

type Claims struct {
	UserID     string `json:"uid"`
	CompanyID  string `json:"cid"`
	MerchantID string `json:"mid"`
	Username   string `json:"username"`
	DeviceID   string `json:"did"`
	Purpose    string `json:"purpose"`
	// ImpersonatedBy is the platform.admin_user.id that requested this session
	// via POST /platform/impersonate — empty for every ordinary tenant login.
	// Checked by AppLockMiddleware (internal/server/applock.go) to skip
	// idle-lock enforcement, since an impersonating admin has no way to
	// unlock with the target user's own PIN. See
	// docs/design/specs/2026-09-24-platform-admin-impersonate-design.md §4.
	ImpersonatedBy string `json:"imp,omitempty"`
	jwt.RegisteredClaims
}

// GenerateToken issues a short-lived access token. ttl comes from
// config.Config.AccessTokenTTLMinutes — see docs/design/specs/2026-09-15-session-refresh-token-design.md.
// deviceID is the same value stored on core.refresh_token.device_id (stable across
// refresh rotations) — embedded here so protected routes can identify a device
// without a new header, see docs/design/specs/2026-09-15-pin-unlock-design.md §4.
func GenerateToken(secret, userID, companyID, merchantID, username, deviceID string, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:     userID,
		CompanyID:  companyID,
		MerchantID: merchantID,
		Username:   username,
		DeviceID:   deviceID,
		Purpose:    accessTokenPurpose,
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
	if claims.Purpose != accessTokenPurpose {
		return nil, errors.New("not an access token")
	}
	return claims, nil
}

// platformAdminAccessPurpose marks a platform-admin token — completely separate
// from accessTokenPurpose ("access", tenant staff). ParsePlatformAdminToken
// rejects anything else, and ParseToken (tenant) rejects this purpose too (its
// own check is claims.Purpose != accessTokenPurpose) — the two token systems
// can never be used interchangeably. See
// docs/design/specs/2026-09-24-platform-admin-foundation-design.md §4.
const platformAdminAccessPurpose = "platform_admin_access"

// PlatformAdminClaims identifies an authenticated platform.admin_user — no
// company_id/merchant_id at all, unlike Claims above, because platform admins
// aren't scoped to any tenant.
type PlatformAdminClaims struct {
	AdminUserID string `json:"aid"`
	Username    string `json:"username"`
	DeviceID    string `json:"did"`
	Purpose     string `json:"purpose"`
	jwt.RegisteredClaims
}

func GeneratePlatformAdminToken(secret, adminUserID, username, deviceID string, ttl time.Duration) (string, error) {
	claims := PlatformAdminClaims{
		AdminUserID: adminUserID,
		Username:    username,
		DeviceID:    deviceID,
		Purpose:     platformAdminAccessPurpose,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ErrNotPlatformAdminToken means a token parsed successfully but wasn't issued
// by GeneratePlatformAdminToken (wrong purpose claim) — includes an ordinary
// tenant access token presented at a platform-admin route.
var ErrNotPlatformAdminToken = errors.New("not a platform-admin token")

func ParsePlatformAdminToken(secret, tokenString string) (*PlatformAdminClaims, error) {
	claims := &PlatformAdminClaims{}
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
	if claims.Purpose != platformAdminAccessPurpose {
		return nil, ErrNotPlatformAdminToken
	}
	return claims, nil
}

// GenerateImpersonationToken issues an access token that authenticates AS
// userID (the impersonation target) — same Claims shape and Purpose as
// GenerateToken, so every existing tenant middleware/handler works
// unmodified, but tagged with impersonatedBy (platform.admin_user.id) so
// AppLockMiddleware can recognize and bypass idle-lock for it.
func GenerateImpersonationToken(secret, userID, companyID, merchantID, username, deviceID, impersonatedBy string, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:         userID,
		CompanyID:      companyID,
		MerchantID:     merchantID,
		Username:       username,
		DeviceID:       deviceID,
		Purpose:        accessTokenPurpose,
		ImpersonatedBy: impersonatedBy,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}
