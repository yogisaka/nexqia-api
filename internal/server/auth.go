// internal/server/auth.go
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mail"
	"github.com/yogisaka/nexqia-api/internal/mfa"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
	"github.com/yogisaka/nexqia-api/internal/session"
)

const (
	authUserIDContextKey         = "auth_user_id"
	authCompanyIDContextKey      = "auth_company_id"
	authMerchantIDContextKey     = "auth_merchant_id"
	authRoleIDContextKey         = "auth_role_id"
	authDeviceIDContextKey       = "auth_device_id"
	authImpersonatedByContextKey = "auth_impersonated_by"

	refreshCookieName = "refresh_token"
	// Must match the actual mounted prefix of EVERY route that reads this cookie —
	// browsers only send a cookie back when the request path matches or is nested
	// under its Set-Cookie Path, so a narrower value here silently breaks whichever
	// consumer route falls outside it. [2026-09-24, bug ketemu live] Was
	// "/api/v1/auth" (covers /auth/refresh, /switch-merchant, /logout fine), but
	// CreateMerchantHandler (0b, spec §3.5) ALSO reads this cookie to reissue a
	// merchant-scoped token — and it's mounted at /api/v1/merchants, a SIBLING of
	// /api/v1/auth, not a sub-path — so the cookie never arrived there at all,
	// every "create first merchant" call failed with "missing refresh token".
	// Widened to the whole API surface so any future consumer route doesn't repeat
	// this; the cookie itself is still HttpOnly, so broadening Path doesn't expose
	// it to JS, it just changes which backend routes receive it automatically.
	refreshCookiePath = "/api/v1"

	merchantSelectionTokenTTL = 5 * time.Minute
)

// Permission codes checked via RequirePermission — must exist in core.permission
// (seeded/migrated, see docs/07-core-ddl.md §2) and be granted through core.role_permission.
const (
	PermCompanyManage    = "core.company.manage"
	PermCompanyManageOwn = "core.company.manage.own"
	PermMerchantManage   = "core.merchant.manage"
	PermUserManage       = "core.user.manage"
	PermRoleManage       = "core.role.manage"
	PermAuditLogView     = "audit.log.view"
)

// AuthMiddleware validates the Bearer JWT issued by LoginHandler and binds the
// authenticated user_id/company_id into the request context. It must run after
// TenantMiddleware (needs TxFromContext for nothing here, but shares the request tx).
func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
			return
		}
		claims, err := auth.ParseToken(secret, token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		userID, ok := parseUUID(claims.UserID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token subject"})
			return
		}
		companyID, ok := parseUUID(claims.CompanyID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token company"})
			return
		}
		c.Set(authUserIDContextKey, userID)
		c.Set(authCompanyIDContextKey, companyID)
		c.Set(authDeviceIDContextKey, claims.DeviceID)
		c.Set(authImpersonatedByContextKey, claims.ImpersonatedBy)
		// Active merchant + active role straight from the claims (no DB query —
		// spec 2026-10-01-active-role-scope-design §3.4). Absent/empty claims
		// simply leave the keys unset, which AuthMerchantID/AuthRoleID report as
		// an invalid pgtype.UUID — permission helpers then fall back to the
		// default role (legacy tokens without "rid", impersonation sessions).
		if merchantID, ok := parseUUID(claims.MerchantID); ok {
			c.Set(authMerchantIDContextKey, merchantID)
		}
		if roleID, ok := parseUUID(claims.RoleID); ok {
			c.Set(authRoleIDContextKey, roleID)
		}
		// Audit trail (spec 2026-09-29-audit-trail §3): tell core.trg_audit_row() who is
		// writing. Transaction-scoped; every group using AuthMiddleware opens the tx first.
		if txVal, ok := c.Get(txContextKey); ok {
			if _, err := txVal.(pgx.Tx).Exec(c.Request.Context(),
				"SELECT set_config('app.current_user_id', $1, true), set_config('app.platform_admin_id', $2, true)",
				userID.String(), claims.ImpersonatedBy); err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set audit context"})
				return
			}
		}
		c.Next()
	}
}

// AuthUserID returns the authenticated caller's user id. Only valid on routes behind AuthMiddleware.
func AuthUserID(c *gin.Context) pgtype.UUID {
	return c.MustGet(authUserIDContextKey).(pgtype.UUID)
}

// AuthCompanyID returns the authenticated caller's company id. Only valid on routes behind AuthMiddleware.
func AuthCompanyID(c *gin.Context) pgtype.UUID {
	return c.MustGet(authCompanyIDContextKey).(pgtype.UUID)
}

// AuthMerchantID returns the active merchant embedded in the access token
// (claims "mid", spec 2026-10-01-active-role-scope-design §3.4). Invalid for
// merchant-less sessions (fresh registration) and for impersonation tokens,
// which carry no mid.
func AuthMerchantID(c *gin.Context) pgtype.UUID {
	if v, ok := c.Get(authMerchantIDContextKey); ok {
		return v.(pgtype.UUID)
	}
	return pgtype.UUID{}
}

// AuthRoleID returns the active role embedded in the access token (claims
// "rid"). Invalid when the token predates the active-role rollout or the
// session runs without a role (no assignment at the merchant).
func AuthRoleID(c *gin.Context) pgtype.UUID {
	if v, ok := c.Get(authRoleIDContextKey); ok {
		return v.(pgtype.UUID)
	}
	return pgtype.UUID{}
}

// AuthDeviceID returns the authenticated caller's device id (spec §4). Only valid
// on routes behind AuthMiddleware.
func AuthDeviceID(c *gin.Context) string {
	return c.MustGet(authDeviceIDContextKey).(string)
}

// AuthImpersonatedBy returns the platform.admin_user.id that started this
// session via POST /platform/impersonate, or "" for an ordinary tenant
// session. Only valid on routes behind AuthMiddleware.
func AuthImpersonatedBy(c *gin.Context) string {
	return c.MustGet(authImpersonatedByContextKey).(string)
}

// rejectDuringImpersonation blocks self-service account writes on an
// impersonation session (platform admin acting as the user): the admin must
// not change the user's contact data, credentials, PIN, MFA or sessions.
// Writes the 403 and returns true when the caller must return immediately.
func rejectDuringImpersonation(c *gin.Context) bool {
	if AuthImpersonatedBy(c) == "" {
		return false
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "not allowed during impersonation"})
	return true
}

// RequirePermission checks the caller holds `code` in the merchant from X-Merchant-ID
// (already required by TenantMiddleware). On failure it writes the response and returns false —
// callers must `return` immediately when this returns false.
//
// Only use this when the request has no single mutation target that could differ from
// the header (e.g. listing, or creating a brand-new resource). When a handler mutates
// an existing resource scoped to a specific merchant, check RequirePermissionForMerchant
// against that resource's actual merchant instead — otherwise a caller with permission
// at merchant A (declared via header) could mutate merchant B's data merely by sharing
// a company, which breaks the per-merchant role isolation merger scenarios require.
func RequirePermission(c *gin.Context, code string) bool {
	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return false
	}
	return RequirePermissionForMerchant(c, code, merchantID)
}

// RequirePermissionForMerchant checks the caller holds `code` at merchantID specifically —
// use this for mutations on an existing resource so the permission check is scoped to the
// resource being mutated, not whatever merchant the caller happened to declare in X-Merchant-ID.
// The check runs against the session's ACTIVE ROLE (spec
// 2026-10-01-active-role-scope-design §3.4): tokens without a "rid" claim
// (legacy tokens, impersonation sessions) fall back to the user's default role
// at merchantID — the role actually being exercised, never the union across
// roles. On failure it writes the response and returns false — callers must
// `return` immediately.
func RequirePermissionForMerchant(c *gin.Context, code string, merchantID pgtype.UUID) bool {
	q := sqlcgen.New(TxFromContext(c))
	roleID := AuthRoleID(c)
	if !roleID.Valid {
		def, err := q.DefaultUserRole(c.Request.Context(), sqlcgen.DefaultUserRoleParams{
			UserID: AuthUserID(c), MerchantID: merchantID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if !def.Valid {
			c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + code})
			return false
		}
		roleID = def
	}
	has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: code, RoleID: roleID,
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if !has {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + code})
		return false
	}
	return true
}

// RequireCompanyLevelPermission checks the caller holds ANY of `codes` — via a
// company-wide core.user_company_role grant, OR a core.user_merchant_role grant
// at ANY merchant of their company (regression path for existing merchant-scoped
// role holders, e.g. seeded Admin — see
// 2026-09-23-saas-registration-owner-bootstrap-design.md §3 poin 4).
// When the session carries both an active merchant AND an active role (claims
// "mid"+"rid", spec 2026-10-01-active-role-scope-design §3.4) the check is
// scoped to that role instead: the role must still be assigned at the active
// merchant and must itself hold one of `codes`. Sessions without both claims
// (company-only, impersonation, legacy tokens) keep the legacy union check.
// Use this instead of RequirePermission for routes registered under the
// companyOnlyAuthed group (no X-Merchant-ID header, no app.current_merchant_id).
// On failure it writes the response and returns false — callers must `return` immediately.
func RequireCompanyLevelPermission(c *gin.Context, codes ...string) bool {
	q := sqlcgen.New(TxFromContext(c))
	roleID := AuthRoleID(c)
	merchantID := AuthMerchantID(c)
	if roleID.Valid && merchantID.Valid {
		assigned, err := q.UserRoleAssigned(c.Request.Context(), sqlcgen.UserRoleAssignedParams{
			UserID: AuthUserID(c), MerchantID: merchantID, RoleID: roleID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if !assigned {
			c.JSON(http.StatusForbidden, gin.H{"error": "missing permission"})
			return false
		}
		has, err := q.RoleHasAnyPermission(c.Request.Context(), sqlcgen.RoleHasAnyPermissionParams{
			RoleID: roleID, Codes: codes,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "missing permission"})
			return false
		}
		return true
	}
	has, err := q.UserHasCompanyLevelPermission(c.Request.Context(), sqlcgen.UserHasCompanyLevelPermissionParams{
		UserID: AuthUserID(c), CompanyID: AuthCompanyID(c), Codes: codes,
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if !has {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing permission"})
		return false
	}
	return true
}

// sessionConfig adapts config.Config's env-driven tunables into session.Config.
func sessionConfig(cfg config.Config) session.Config {
	return session.Config{
		JWTSecret:      cfg.JWTSecret,
		AccessTokenTTL: time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute,
		IdleTimeout:    time.Duration(cfg.RefreshTokenIdleTimeoutHours) * time.Hour,
		AbsoluteTTL:    time.Duration(cfg.RefreshTokenAbsoluteTTLDays) * 24 * time.Hour,
	}
}

func setRefreshCookie(c *gin.Context, cfg config.Config, token string, expiresAt time.Time) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, token, int(time.Until(expiresAt).Seconds()), refreshCookiePath, "", cfg.CookieSecure, true)
}

func clearRefreshCookie(c *gin.Context, cfg config.Config) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, "", -1, refreshCookiePath, "", cfg.CookieSecure, true)
}

// deviceHeaders reads the client-generated device identity (spec §6): X-Device-Id
// is required (client persists it in localStorage across sessions), device_label
// is derived from User-Agent since parsing it further needs no extra dependency.
func deviceHeaders(c *gin.Context) (deviceID, deviceLabel string, ip netip.Addr, err error) {
	deviceID = c.GetHeader("X-Device-Id")
	if deviceID == "" {
		return "", "", netip.Addr{}, errors.New("X-Device-Id header is required")
	}
	deviceLabel = c.GetHeader("User-Agent")
	if deviceLabel == "" {
		deviceLabel = "Unknown device"
	} else if len(deviceLabel) > 255 {
		deviceLabel = deviceLabel[:255]
	}
	if parsed, parseErr := netip.ParseAddr(c.ClientIP()); parseErr == nil {
		ip = parsed
	} else {
		ip = netip.IPv4Unspecified()
	}
	return deviceID, deviceLabel, ip, nil
}

// checkDeviceLimit enforces core.company.max_concurrent_sessions (spec §6/§8):
// exceeding it is an explicit 409, not a silent auto-kick of the oldest device.
// On failure it writes the response and returns false.
func checkDeviceLimit(c *gin.Context, q *sqlcgen.Queries, userID, companyID pgtype.UUID) bool {
	ctx := c.Request.Context()
	max, err := q.GetCompanyMaxConcurrentSessions(ctx, companyID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	count, err := q.CountActiveRefreshTokens(ctx, sqlcgen.CountActiveRefreshTokensParams{UserID: userID, CompanyID: companyID})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if count >= int64(max) {
		active, _ := q.ListActiveRefreshTokens(ctx, userID)
		c.JSON(http.StatusConflict, gin.H{"error": "device limit reached", "data": gin.H{"active_sessions": active}})
		return false
	}
	return true
}

// mfaNudgeFlagKey is the core.feature_flag key a merchant can enable to nudge
// (never block) users who haven't enrolled TOTP yet (spec §3) — soft, informational
// only, distinct from mfaEnabled below which is per-user and does block.
const mfaNudgeFlagKey = "auth.require_totp"

// checkMFANudge reports whether merchantID has the soft-nudge flag on. LoginHandler
// and SelectMerchantHandler run under CompanyOnlyMiddleware, where
// app.current_merchant_id is deliberately left unset (see tenant.go) — this sets it
// locally (third set_config arg true = transaction-scoped) for this one lookup only,
// the same pattern TenantMiddleware uses for merchant-scoped routes.
func checkMFANudge(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) bool {
	ctx := c.Request.Context()
	if _, err := TxFromContext(c).Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID.String()); err != nil {
		return false
	}
	flag, err := q.GetFeatureFlag(ctx, sqlcgen.GetFeatureFlagParams{MerchantID: merchantID, FlagKey: mfaNudgeFlagKey})
	if err != nil {
		return false
	}
	var enabled bool
	if err := json.Unmarshal(flag.FlagValue, &enabled); err != nil {
		return false
	}
	return enabled
}

// verifyLoginTOTP checks code against the user's decrypted TOTP secret, falling
// back to their one-time recovery codes (spec §4, §7). A matched recovery code is
// marked used so it can't be replayed.
func verifyLoginTOTP(c *gin.Context, q *sqlcgen.Queries, enc *mfa.Encryptor, hasher *auth.PasswordHasher, userID pgtype.UUID, encryptedSecret, code string) bool {
	ctx := c.Request.Context()
	secret, err := enc.Decrypt(encryptedSecret)
	if err == nil && mfa.Validate(code, secret) {
		return true
	}
	recoveryCodes, err := q.ListActiveMFARecoveryCodes(ctx, userID)
	if err != nil {
		return false
	}
	for _, rc := range recoveryCodes {
		ok, err := hasher.Verify(ctx, rc.CodeHash, code)
		if err == nil && ok {
			_ = q.MarkMFARecoveryCodeUsed(ctx, rc.ID)
			return true
		}
	}
	return false
}

// getPinLockFlagForMerchant is getPinLockFlag for callers where merchantID isn't
// necessarily the one TenantMiddleware already set app.current_merchant_id to —
// login/select-merchant (still under CompanyOnlyMiddleware, active merchant is
// only just now being chosen) and switch-merchant (TenantMiddleware set it to the
// OLD merchant from X-Merchant-ID, not the new target). Same set_config
// workaround as checkMFANudge above, for the same reason.
func getPinLockFlagForMerchant(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) pinLockFlag {
	if _, err := TxFromContext(c).Exec(c.Request.Context(), "SELECT set_config('app.current_merchant_id', $1, true)", merchantID.String()); err != nil {
		return pinLockFlag{}
	}
	return getPinLockFlag(c, q, merchantID)
}

// issueLoginSession finishes login once the active merchant is known (either the
// user's only merchant, or one chosen via SelectMerchantHandler) — shared by both.
// user is the caller's already-loaded core.app_user row: mfa_secret drives the
// mfa_nudge signal, pin_hash drives pin_nudge and — if the merchant's auth.pin_lock
// flag is enabled and the user already has a PIN — seeds the applock Redis key so
// the very first request after login isn't spuriously treated as locked.
func issueLoginSession(c *gin.Context, cfg config.Config, q *sqlcgen.Queries, redisClient *redis.Client, user sqlcgen.CoreAppUser, merchantID pgtype.UUID) {
	decision, graceUntil, err := tenantMFAGrace(c, q, cfg, user, merchantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate MFA policy"})
		return
	}
	if decision == mfaGraceExpired {
		payload, err := mfaSetupRequiredPayload(cfg.JWTSecret, user.ID.String(), auth.MFAEnrollmentPurpose, mfaTOTPIssuer, user.Username)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue enrollment token"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
		return
	}
	deviceID, deviceLabel, ip, err := deviceHeaders(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !checkDeviceLimit(c, q, user.ID, user.CompanyID) {
		return
	}
	// Checked before session.Issue records this device in core.refresh_token.
	deviceSeen := loginDeviceSeen(c, user.ID, deviceID)
	issued, err := session.Issue(c.Request.Context(), q, sessionConfig(cfg), session.IssueParams{
		UserID: user.ID, CompanyID: user.CompanyID, MerchantID: merchantID,
		Username: user.Username, DeviceID: deviceID, DeviceLabel: deviceLabel, IP: ip,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue session"})
		return
	}
	// Only after the session exists: a login that fails later must not alert.
	if !deviceSeen && user.Email.Valid && user.Email.String != "" {
		enqueueNewDeviceAlert(c, cfg, user, deviceLabel, ip)
	}
	setRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
	data := gin.H{
		"token":       issued.AccessToken,
		"expires_in":  int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
		"merchant_id": merchantID.String(),
		"user_id":     user.ID.String(),
		"username":    user.Username,
		// Active role for this session — string UUID, or null when the user has
		// no role assigned at the merchant (spec
		// 2026-10-01-active-role-scope-design §3.2).
		"active_role_id": uuidOrNil(issued.ActiveRoleID),
	}
	// Topbar identity (spec: current user needs a real name/photo, not the
	// company name) — person_id is nullable (system/API-only accounts have none).
	if user.PersonID.Valid {
		if person, err := q.GetPersonByID(c.Request.Context(), user.PersonID); err == nil {
			data["full_name"] = person.FullName
			if person.PhotoUrl.Valid {
				data["photo_url"] = person.PhotoUrl.String
			}
		}
	}
	// The grace decision was already computed above — surface its deadline so
	// the frontend can remind the user to enroll before enforcement starts.
	if decision == mfaGraceActive {
		data["mfa_setup_due_at"] = graceUntil.Format(time.RFC3339)
	}
	if !user.MfaSecret.Valid && checkMFANudge(c, q, merchantID) {
		data["mfa_nudge"] = true
	}
	if pinFlag := getPinLockFlagForMerchant(c, q, merchantID); pinFlag.Enabled {
		if user.PinHash.Valid {
			idleMinutes := pinLockIdleMinutes(cfg, pinFlag)
			_ = redisClient.Set(c.Request.Context(), applockKey(user.ID, deviceID), "1", time.Duration(idleMinutes)*time.Minute).Err()
			// Lets the frontend mirror the server's idle window with its own
			// window-activity timer instead of guessing a value — see
			// nexqia-his src/auth/IdleLock.tsx.
			data["pin_lock_idle_minutes"] = idleMinutes
		} else {
			data["pin_nudge"] = true
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// loginDeviceSeen reports whether the user ever had a session on deviceID
// (core.user_device_seen, any status). It runs in a savepoint: a failed check
// must neither block the login nor abort the request transaction, so on error
// the device is treated as seen (no alert).
func loginDeviceSeen(c *gin.Context, userID pgtype.UUID, deviceID string) bool {
	ctx := c.Request.Context()
	sp, err := TxFromContext(c).Begin(ctx)
	if err != nil {
		slog.Warn("new-device check skipped", "user_id", userID.String(), "error", err)
		return true
	}
	seen, err := sqlcgen.New(sp).UserDeviceSeen(ctx, sqlcgen.UserDeviceSeenParams{UserID: userID, DeviceID: deviceID})
	if err != nil {
		_ = sp.Rollback(ctx)
		slog.Warn("new-device check failed", "user_id", userID.String(), "error", err)
		return true
	}
	if err := sp.Commit(ctx); err != nil {
		slog.Warn("new-device check release failed", "user_id", userID.String(), "error", err)
	}
	return seen
}

// enqueueNewDeviceAlert queues the new-device login email (spec
// 2026-10-01-email-outbox §3.5) in a savepoint of the login transaction: an
// alert that cannot be queued is rolled back alone and the login still succeeds.
func enqueueNewDeviceAlert(c *gin.Context, cfg config.Config, user sqlcgen.CoreAppUser, deviceLabel string, ip netip.Addr) {
	ctx := c.Request.Context()
	msg, err := mail.RenderNewDeviceLogin(mail.NewDeviceData{
		Username:    user.Username,
		When:        time.Now().In(cfg.AuditLocation).Format("02 Jan 2006 15:04 MST"),
		Device:      deviceLabel,
		IP:          ip.String(),
		SecurityURL: cfg.AppBaseURL + "/account/security",
	})
	if err != nil {
		slog.Warn("new-device alert not queued", "user_id", user.ID.String(), "error", err)
		return
	}
	sp, err := TxFromContext(c).Begin(ctx)
	if err != nil {
		slog.Warn("new-device alert not queued", "user_id", user.ID.String(), "error", err)
		return
	}
	if _, err := sqlcgen.New(sp).EnqueueEmail(ctx, sqlcgen.EnqueueEmailParams{
		CompanyID: user.CompanyID, Kind: "new_device_login", ToAddress: user.Email.String,
		Subject: msg.Subject, BodyText: msg.Text, BodyHtml: msg.HTML,
	}); err != nil {
		_ = sp.Rollback(ctx)
		slog.Warn("new-device alert not queued", "user_id", user.ID.String(), "error", err)
		return
	}
	if err := sp.Commit(ctx); err != nil {
		slog.Warn("new-device alert savepoint release failed", "user_id", user.ID.String(), "error", err)
	}
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	TotpCode string `json:"totp_code"`
}

// LoginHandler authenticates against core.app_user scoped to the X-Company-ID header
// (RLS-enforced, must run behind CompanyOnlyMiddleware — the active merchant isn't
// known yet), enforces per-username/IP sliding-window rate limiting before any
// password comparison, and — once the active merchant is resolved — issues a JWT
// access token plus an HttpOnly refresh-token cookie (spec §3a, §6).
// On successful login with a legacy bcrypt hash, transparently rehashes to Argon2id
// (see 2026-09-15-ratelimit-hardening-design.md §11).
// LoginHandler godoc
// @Summary Log in
// @Description Rate-limited by username/IP. Returns totp_required if the account has TOTP enabled and no/invalid code was given, or requires_merchant_selection if the user belongs to more than one merchant.
// @Tags auth
// @Accept json
// @Produce json
// @Param X-Company-ID header string true "Company UUID"
// @Param request body loginRequest true "Credentials"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /auth/login [post]
func LoginHandler(secret string, limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher, enc *mfa.Encryptor, redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		companyID, ok := parseUUID(c.GetHeader("X-Company-ID"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Company-ID"})
			return
		}
		var req loginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !checkLoginRateLimit(c, limiter, cfg, req.Username) {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		company, err := q.GetCompanyByID(c.Request.Context(), companyID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid company"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if !company.IsActive {
			c.JSON(http.StatusForbidden, gin.H{"error": "this company account is suspended, contact support"})
			return
		}
		user, err := q.GetAppUserByUsername(c.Request.Context(), sqlcgen.GetAppUserByUsernameParams{
			CompanyID: companyID, Username: req.Username,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), user.PasswordHash, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			respondInternalError(c, err)
			return
		}
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		if hasher.NeedsRehash(user.PasswordHash) {
			if newHash, rehashErr := hasher.Hash(c.Request.Context(), req.Password); rehashErr == nil {
				_ = q.UpdateAppUserPassword(c.Request.Context(), sqlcgen.UpdateAppUserPasswordParams{
					ID: user.ID, PasswordHash: newHash, UpdatedBy: user.ID,
				})
			}
		}
		_ = q.TouchAppUserLastLogin(c.Request.Context(), user.ID)

		// TOTP check happens once here, before merchant enumeration — password is
		// already confirmed above, so it's safe to reveal 2FA-enrollment status now
		// (spec §4). SelectMerchantHandler needs no separate check as a result.
		if user.MfaSecret.Valid {
			if req.TotpCode == "" {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{"totp_required": true}, "meta": gin.H{}})
				return
			}
			if !verifyLoginTOTP(c, q, enc, hasher, user.ID, user.MfaSecret.String, req.TotpCode) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
				return
			}
		}

		merchants, err := session.ListUserMerchants(c.Request.Context(), TxFromContext(c), user.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if len(merchants) == 0 {
			// [2026-09-24, bug ketemu live] DULU 403 di sini — tapi 0 merchant itu
			// state NORMAL buat Owner yang baru self-register (0a) dan belum bikin
			// merchant pertamanya (0b) — RegisterHandler SENDIRI udah nerbitin token
			// merchant-less (session.Issue MerchantID: pgtype.UUID{}) pas auto-login,
			// tapi LoginHandler ini gak pernah disamain buat LOGIN ULANG-nya: Owner
			// yang logout sebelum bikin merchant jadi TERKUNCI TOTAL, gak bisa login
			// lagi walau username/password/company code semuanya benar. Frontend
			// (RequireAuth.tsx) udah siap nanganin auth.merchantId === null (redirect
			// ke /settings/merchants) — cukup issue session merchant-less yang sama
			// kayak RegisterHandler, jangan tolak di sini.
			issueLoginSession(c, cfg, q, redisClient, user, pgtype.UUID{})
			return
		}
		if len(merchants) > 1 {
			selToken, err := auth.GenerateMerchantSelectionToken(secret, user.ID.String(), merchantSelectionTokenTTL)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue selection token"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": gin.H{
				"requires_merchant_selection": true,
				"selection_token":             selToken,
				"merchants":                   merchants,
			}, "meta": gin.H{}})
			return
		}

		issueLoginSession(c, cfg, q, redisClient, user, merchants[0].ID)
	}
}

type selectMerchantRequest struct {
	SelectionToken string `json:"selection_token" binding:"required"`
	MerchantID     string `json:"merchant_id" binding:"required"`
}

// SelectMerchantHandler completes a login that LoginHandler paused for merchant
// selection (spec §3a) — consumes the short-lived selection token instead of
// asking for the password again, validates the chosen merchant against
// core.user_merchant_role, then issues the session exactly like a single-merchant
// login would. Must run behind CompanyOnlyMiddleware (same X-Company-ID as login).
// SelectMerchantHandler godoc
// @Summary Complete login by selecting a merchant
// @Description Used after LoginHandler returns requires_merchant_selection — consumes the short-lived selection_token instead of re-sending a password.
// @Tags auth
// @Accept json
// @Produce json
// @Param X-Company-ID header string true "Company UUID"
// @Param request body selectMerchantRequest true "Selection token + merchant_id"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /auth/select-merchant [post]
func SelectMerchantHandler(secret string, cfg config.Config, redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req selectMerchantRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		claims, err := auth.ParseMerchantSelectionToken(secret, req.SelectionToken)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired selection token"})
			return
		}
		userID, ok := parseUUID(claims.UserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid selection token subject"})
			return
		}
		merchantID, ok := parseUUID(req.MerchantID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
			return
		}
		has, err := session.MerchantIsAssigned(c.Request.Context(), TxFromContext(c), userID, merchantID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "user is not assigned to this merchant"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		issueLoginSession(c, cfg, q, redisClient, user, merchantID)
	}
}

// RefreshHandler rotates the refresh-token cookie and reissues an access token
// (spec §7). Must run behind CompanyOnlyMiddleware (needs X-Company-ID for
// core.refresh_token's RLS; the active merchant is read back off the stored row).
// RefreshHandler godoc
// @Summary Rotate access token
// @Description Reads the refresh_token HttpOnly cookie, rotates it, and reissues an access token.
// @Tags auth
// @Produce json
// @Param X-Company-ID header string true "Company UUID"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /auth/refresh [post]
func RefreshHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		ip, parseErr := netip.ParseAddr(c.ClientIP())
		if parseErr != nil {
			ip = netip.IPv4Unspecified()
		}
		q := sqlcgen.New(TxFromContext(c))
		issued, err := session.Refresh(c.Request.Context(), q, sessionConfig(cfg), rawToken, ip)
		if err != nil {
			clearRefreshCookie(c, cfg)
			switch {
			case errors.Is(err, session.ErrNoSession), errors.Is(err, session.ErrSessionExpired), errors.Is(err, session.ErrSessionRevoked):
				// session.Refresh revokes the device chain (reuse) or the expired
				// token before returning these errors — security effects that must
				// persist despite the 401 (spec 2026-10-06-request-tx-finalization §4 #2).
				KeepTxOnError(c)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				respondInternalError(c, err)
			}
			return
		}
		setRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"token":          issued.AccessToken,
			"expires_in":     int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
			"active_role_id": uuidOrNil(issued.ActiveRoleID),
		}, "meta": gin.H{}})
	}
}

// LogoutHandler revokes the session tied to the refresh-token cookie. Deliberately
// does not require a valid access token (a user with an expired access token but
// still-valid refresh token must still be able to log out) — runs behind
// CompanyOnlyMiddleware only.
// LogoutHandler godoc
// @Summary Log out
// @Description Revokes the session tied to the refresh_token cookie. Does not require a valid access token.
// @Tags auth
// @Produce json
// @Param X-Company-ID header string true "Company UUID"
// @Success 200 {object} apiResponse
// @Router /auth/logout [post]
func LogoutHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(refreshCookieName)
		if err == nil && rawToken != "" {
			q := sqlcgen.New(TxFromContext(c))
			_ = session.RevokeByRawToken(c.Request.Context(), q, rawToken)
		}
		clearRefreshCookie(c, cfg)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}

type switchMerchantRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
}

// SwitchMerchantHandler lets an already-logged-in user change their session's
// active merchant without re-entering a password (spec §3a) — only the access
// token is reissued, the refresh token is not rotated. Runs behind AuthMiddleware.
// SwitchMerchantHandler godoc
// @Summary Switch active merchant
// @Description Reissues the access token bound to a different merchant the user is assigned to, without re-authenticating.
// @Tags auth
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body switchMerchantRequest true "Target merchant_id"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/switch-merchant [post]
func SwitchMerchantHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req switchMerchantRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		merchantID, ok := parseUUID(req.MerchantID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
			return
		}
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		userID := AuthUserID(c)
		has, err := session.MerchantIsAssigned(c.Request.Context(), TxFromContext(c), userID, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to switch merchant"})
			return
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "user is not assigned to this merchant"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to switch merchant"})
			return
		}
		// Same policy gate as login, but for the TARGET merchant — switching
		// into a merchant whose auth.require_totp policy has expired must not
		// hand out a merchant-scoped session either.
		decision, _, err := tenantMFAGrace(c, q, cfg, user, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate MFA policy"})
			return
		}
		if decision == mfaGraceExpired {
			c.JSON(http.StatusForbidden, gin.H{"error": "mfa setup required", "code": "mfa_setup_required"})
			return
		}
		token, activeRoleID, err := session.SwitchMerchant(c.Request.Context(), q, sessionConfig(cfg), rawToken, userID, merchantID)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrNoSession), errors.Is(err, session.ErrSessionRevoked), errors.Is(err, session.ErrSessionMismatch):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				// UpdateRefreshTokenMerchant already wrote — abort, don't plain-500.
				abortInternalError(c, err)
			}
			return
		}
		data := gin.H{
			"token":       token,
			"expires_in":  int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
			"merchant_id": merchantID.String(),
			"user_id":     userID.String(),
			// Switching merchants resets the active role to the default at the
			// NEW merchant (session.SwitchMerchant, spec §3.3).
			"active_role_id": uuidOrNil(activeRoleID),
		}
		// Mirrors issueLoginSession's pin_lock_idle_minutes (§ getPinLockFlagForMerchant) —
		// without this the frontend kept whatever idle window the PREVIOUS merchant had,
		// wrong whenever the new merchant's auth.pin_lock config differs.
		if pinFlag := getPinLockFlagForMerchant(c, q, merchantID); pinFlag.Enabled {
			if user, err := q.GetAppUserByID(c.Request.Context(), userID); err == nil && user.PinHash.Valid {
				data["pin_lock_idle_minutes"] = pinLockIdleMinutes(cfg, pinFlag)
			}
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
	}
}

type switchRoleRequest struct {
	RoleID string `json:"role_id" binding:"required"`
}

// SwitchRoleHandler lets an already-logged-in user change their session's
// active role (within the current merchant) without re-entering a password —
// only the access token is reissued, the refresh token is not rotated (spec
// 2026-10-01-active-role-scope-design §3.3). Runs behind AuthMiddleware; the
// refresh-token cookie identifies the session whose active_role_id is updated,
// so /auth/refresh keeps the chosen role.
// SwitchRoleHandler godoc
// @Summary Switch active role
// @Description Reissues the access token bound to a different role assigned to the user at the active merchant, without re-authenticating.
// @Tags auth
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body switchRoleRequest true "Target role_id"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/switch-role [post]
func SwitchRoleHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req switchRoleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		roleID, ok := parseUUID(req.RoleID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role_id"})
			return
		}
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		userID := AuthUserID(c)
		token, err := session.SwitchRole(c.Request.Context(), sqlcgen.New(TxFromContext(c)), sessionConfig(cfg), rawToken, userID, roleID)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrNoSession), errors.Is(err, session.ErrSessionRevoked), errors.Is(err, session.ErrSessionMismatch):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			case errors.Is(err, session.ErrRoleNotAssigned):
				c.JSON(http.StatusForbidden, gin.H{"error": "role not assigned"})
			default:
				// UpdateRefreshTokenActiveRole may already have written — abort.
				abortInternalError(c, err)
			}
			return
		}
		data := gin.H{
			"token":      token,
			"expires_in": int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
			"user_id":    userID.String(),
		}
		if merchantID := AuthMerchantID(c); merchantID.Valid {
			data["merchant_id"] = merchantID.String()
		}
		data["active_role_id"] = roleID.String()
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
	}
}

// ListSessionsHandler lists the caller's active devices (spec §6, for a
// "log out this device" UI). Runs behind AuthMiddleware.
// ListSessionsHandler godoc
// @Summary List active sessions/devices
// @Tags auth
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /auth/sessions [get]
func ListSessionsHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListActiveRefreshTokens(c.Request.Context(), AuthUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions"})
		return
	}
	currentDevice := AuthDeviceID(c)
	sessions := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		sessions = append(sessions, gin.H{
			"id": r.ID, "device_label": r.DeviceLabel, "merchant_id": r.MerchantID,
			"issued_at": r.IssuedAt, "last_used_at": r.LastUsedAt,
			"is_current": r.DeviceID == currentDevice,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": sessions, "meta": gin.H{}})
}

// RevokeSessionHandler revokes one of the caller's own devices by refresh-token
// row id — ownership-checked so a user can only revoke their own sessions.
// Runs behind AuthMiddleware.
// RevokeSessionHandler godoc
// @Summary Revoke one of the caller's own sessions/devices
// @Tags auth
// @Produce json
// @Security BearerAuth
// @Param id path string true "Session (refresh token) UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /auth/sessions/{id} [delete]
func RevokeSessionHandler(c *gin.Context) {
	if rejectDuringImpersonation(c) {
		return
	}
	sessionID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	row, err := q.GetRefreshTokenByID(c.Request.Context(), sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if row.UserID != AuthUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if row.DeviceID == AuthDeviceID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "use logout for the current device"})
		return
	}
	if err := q.RevokeRefreshToken(c.Request.Context(), sessionID); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}

var (
	registerUsernameFormat = regexp.MustCompile(`^[a-z0-9.]+$`)
	registerPhoneFormat    = regexp.MustCompile(`^(\+62|0)[0-9]{9,13}$`)
	registerEmailFormat    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

type registerRequest struct {
	CompanyName          string `json:"company_name" binding:"required,min=3"`
	FullName             string `json:"full_name" binding:"required"`
	Username             string `json:"username" binding:"required"`
	Email                string `json:"email" binding:"required"`
	Phone                string `json:"phone" binding:"required"`
	Password             string `json:"password" binding:"required,min=8"`
	PasswordConfirmation string `json:"password_confirmation" binding:"required"`
}

func (r registerRequest) validate() string {
	switch {
	case !registerUsernameFormat.MatchString(r.Username):
		return "username must contain only lowercase letters, digits, and dots"
	case !registerEmailFormat.MatchString(r.Email):
		return "invalid email format"
	case !registerPhoneFormat.MatchString(r.Phone):
		return "invalid phone number format"
	case r.Password != r.PasswordConfirmation:
		return "password and password_confirmation do not match"
	default:
		return ""
	}
}

// RegisterHandler godoc
// @Summary Self-register a new company and its Owner account
// @Description Public, unauthenticated. Creates a company, an Owner person/app_user,
// @Description a bootstrap "Owner" role with every permission except the platform-wide
// @Description core.company.manage, and auto-logs in. See
// @Description 2026-09-23-saas-registration-owner-bootstrap-design.md.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body registerRequest true "Registration data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /auth/register [post]
func RegisterHandler(pool *pgxpool.Pool, cfg config.Config, hasher *auth.PasswordHasher, limiter *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !checkRegisterRateLimit(c, limiter, cfg) {
			return
		}

		var req registerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if msg := req.validate(); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		deviceID, deviceLabel, ip, err := deviceHeaders(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}
		defer tx.Rollback(ctx)
		q := sqlcgen.New(tx)

		// [2026-09-24, bug ketemu live] Pre-check email/phone (CheckAppUserEmailExists/
		// CheckAppUserPhoneExists) DIHAPUS — 2 masalah: (1) query polos ke
		// core.app_user itu RLS-protected (USING app.current_company_id), dijalanin
		// di sini SEBELUM set_config pernah dipanggil di tx ini sama sekali → crash
		// "unrecognized configuration parameter" (SQLSTATE 42704) tiap kali endpoint
		// ini dipanggil; (2) walau dipindah setelah set_config, tetep gak bakal
		// bener — email/phone UNIQUE GLOBAL lintas company (migration 000034), tapi
		// RLS cuma bisa liat 1 company (yang baru dibuat, otomatis 0 baris) →
		// SELALU "gak ada duplikat" walau ada di company lain. Enforcement asli
		// (UNIQUE constraint + isUniqueViolation di CreateAppUser, di bawah) udah
		// cukup DAN benar independen dari RLS — gak perlu pre-check terpisah.

		var companyID pgtype.UUID
		if err := tx.QueryRow(ctx, "SELECT uuid_generate_v7()").Scan(&companyID); err != nil {
			respondInternalError(c, err)
			return
		}

		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID.String()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}
		company, err := InsertWithUniqueCode(ctx, tx, generateCode, func(q *sqlcgen.Queries, code string) (sqlcgen.CoreCompany, error) {
			return q.CreateCompanyWithID(ctx, sqlcgen.CreateCompanyWithIDParams{
				ID: companyID, Code: code, Name: req.CompanyName, CreatedBy: pgtype.UUID{},
			})
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		// Gender sengaja gak diisi (zero-value pgtype.Text{} = NULL) — form
		// registrasi Owner gak ngumpulin data ini (spec §2), core.person.gender
		// nullable sejak migration 000038. Lihat
		// 2026-09-24-fix-0a-0b-0c-review-findings.md Task 3.
		person, err := q.CreatePerson(ctx, sqlcgen.CreatePersonParams{
			CompanyID: companyID,
			FullName:  req.FullName,
			CreatedBy: pgtype.UUID{},
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		hash, err := hasher.Hash(ctx, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}

		user, err := q.CreateAppUser(ctx, sqlcgen.CreateAppUserParams{
			CompanyID: companyID, PersonID: person.ID, Username: req.Username,
			Email:        pgtype.Text{String: req.Email, Valid: true},
			Phone:        pgtype.Text{String: req.Phone, Valid: true},
			PasswordHash: hash, CreatedBy: pgtype.UUID{},
		})
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "email or phone number already registered"})
				return
			}
			respondInternalError(c, err)
			return
		}

		role, err := q.CreateRole(ctx, sqlcgen.CreateRoleParams{
			CompanyID: companyID, Name: "Owner", Description: pgtype.Text{String: "Company owner — full access", Valid: true}, IsSystem: true, CreatedBy: user.ID,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		permissions, err := q.ListPermissions(ctx)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for _, p := range permissions {
			if p.Code == PermCompanyManage {
				continue // §4: platform-wide, never in the Owner bundle
			}
			if err := q.AddRolePermission(ctx, sqlcgen.AddRolePermissionParams{RoleID: role.ID, PermissionID: p.ID}); err != nil {
				respondInternalError(c, err)
				return
			}
		}

		if _, err := q.CreateUserCompanyRole(ctx, sqlcgen.CreateUserCompanyRoleParams{
			UserID: user.ID, CompanyID: companyID, RoleID: role.ID, CreatedBy: user.ID,
		}); err != nil {
			respondInternalError(c, err)
			return
		}

		if !checkDeviceLimit(c, q, user.ID, companyID) {
			return
		}
		issued, err := session.Issue(ctx, q, sessionConfig(cfg), session.IssueParams{
			UserID: user.ID, CompanyID: companyID, MerchantID: pgtype.UUID{},
			Username: user.Username, DeviceID: deviceID, DeviceLabel: deviceLabel, IP: ip,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue session"})
			return
		}

		// Welcome email (company code) is part of the registration contract: it is
		// queued in this transaction, so a rolled-back registration sends nothing
		// and a failure to queue fails the registration (spec 2026-10-01-email-outbox §3.5).
		welcome, err := mail.RenderWelcome(mail.WelcomeData{
			FullName: req.FullName, CompanyName: company.Name, CompanyCode: company.Code,
			Username: user.Username, LoginURL: cfg.AppBaseURL + "/login",
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if _, err := q.EnqueueEmail(ctx, sqlcgen.EnqueueEmailParams{
			CompanyID: companyID, Kind: "welcome", ToAddress: req.Email,
			Subject: welcome.Subject, BodyText: welcome.Text, BodyHtml: welcome.HTML,
		}); err != nil {
			respondInternalError(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit"})
			return
		}

		setRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"token":        issued.AccessToken,
			"expires_in":   int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
			"company_id":   companyID.String(),
			"company_code": company.Code,
			"company_name": company.Name,
			"user_id":      user.ID.String(),
			"username":     user.Username,
			"full_name":    req.FullName,
		}, "meta": gin.H{}})
	}
}
