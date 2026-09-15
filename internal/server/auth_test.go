// internal/server/auth_test.go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/config"
)

func TestDeviceHeaders_RequiresDeviceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	if _, _, _, err := deviceHeaders(c); err == nil {
		t.Error("expected error when X-Device-Id header is missing")
	}
}

func TestDeviceHeaders_DerivesLabelFromUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	c.Request.Header.Set("X-Device-Id", "device-123")
	c.Request.Header.Set("User-Agent", "TestAgent/1.0")

	deviceID, deviceLabel, _, err := deviceHeaders(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deviceID != "device-123" {
		t.Errorf("expected device-123, got %q", deviceID)
	}
	if deviceLabel != "TestAgent/1.0" {
		t.Errorf("expected TestAgent/1.0, got %q", deviceLabel)
	}
}

func TestDeviceHeaders_FallsBackWhenUserAgentMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	c.Request.Header.Set("X-Device-Id", "device-123")

	_, deviceLabel, _, err := deviceHeaders(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deviceLabel != "Unknown device" {
		t.Errorf("expected fallback label, got %q", deviceLabel)
	}
}

func TestSetRefreshCookie_SetsHttpOnlySecureLax(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	cfg := config.Config{CookieSecure: true}
	setRefreshCookie(c, cfg, "raw-token-value", time.Now().Add(time.Hour))

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	ck := cookies[0]
	if ck.Name != refreshCookieName || ck.Value != "raw-token-value" {
		t.Errorf("unexpected cookie name/value: %+v", ck)
	}
	if !ck.HttpOnly || !ck.Secure {
		t.Errorf("expected HttpOnly+Secure cookie, got %+v", ck)
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSite=Lax, got %v", ck.SameSite)
	}
	if ck.Path != refreshCookiePath {
		t.Errorf("expected path %q, got %q", refreshCookiePath, ck.Path)
	}
}

func TestClearRefreshCookie_ExpiresImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	clearRefreshCookie(c, config.Config{CookieSecure: true})

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	if cookies[0].MaxAge >= 0 {
		t.Errorf("expected negative MaxAge to clear cookie, got %d", cookies[0].MaxAge)
	}
}

func TestSessionConfig_ConvertsMinutesHoursDays(t *testing.T) {
	cfg := config.Config{
		JWTSecret:                    "secret",
		AccessTokenTTLMinutes:        15,
		RefreshTokenIdleTimeoutHours: 24,
		RefreshTokenAbsoluteTTLDays:  30,
	}
	sc := sessionConfig(cfg)
	if sc.AccessTokenTTL != 15*time.Minute {
		t.Errorf("expected 15m, got %v", sc.AccessTokenTTL)
	}
	if sc.IdleTimeout != 24*time.Hour {
		t.Errorf("expected 24h, got %v", sc.IdleTimeout)
	}
	if sc.AbsoluteTTL != 30*24*time.Hour {
		t.Errorf("expected 30d, got %v", sc.AbsoluteTTL)
	}
}
