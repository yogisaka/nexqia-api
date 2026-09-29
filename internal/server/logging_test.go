// internal/server/logging_test.go
package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestLogger_OmitsQueryString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	router := gin.New()
	router.Use(Recovery(&buf), RequestLogger(&buf))
	router.GET("/x", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/x?q=Budi%20Santoso&nik=3171", nil)
	req.Header.Set("Cookie", "refresh_token=SECRET")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	logged := buf.String()
	if !strings.Contains(logged, "/x") {
		t.Errorf("log missing path: %q", logged)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	for _, forbidden := range []string{"Budi", "3171", "q=", "SECRET"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("log contains %q: %q", forbidden, logged)
		}
	}
}

func TestRecovery_NoRequestDump(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	router := gin.New()
	router.Use(Recovery(&buf), RequestLogger(&buf))
	router.GET("/boom", func(c *gin.Context) { panic("panic-value-42") })

	req := httptest.NewRequest(http.MethodGet, "/boom?q=Budi", nil)
	req.Header.Set("Cookie", "refresh_token=SECRET")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, `"error":"internal server error"`) {
		t.Errorf("body: got %q", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "panic-value-42") || !strings.Contains(logged, "/boom") {
		t.Errorf("log missing panic value or path: %q", logged)
	}
	for _, forbidden := range []string{"Budi", "SECRET"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("log contains %q: %q", forbidden, logged)
		}
	}
}
