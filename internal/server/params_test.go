// internal/server/params_test.go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseUUID(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"valid", "11111111-1111-1111-1111-111111111111", true},
		{"empty", "", false},
		{"malformed", "not-a-uuid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := parseUUID(tc.input)
			if ok != tc.ok {
				t.Errorf("parseUUID(%q): expected ok=%v, got %v", tc.input, tc.ok, ok)
			}
		})
	}
}

func TestPaginationParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		query      string
		wantLimit  int32
		wantOffset int32
	}{
		{"defaults", "", 50, 0},
		{"custom", "?limit=10&offset=20", 10, 20},
		{"limit too high falls back to default", "?limit=500", 50, 0},
		{"negative offset falls back to default", "?offset=-5", 50, 0},
		{"non-numeric falls back to default", "?limit=abc&offset=xyz", 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tc.query, nil)

			limit, offset := paginationParams(c)
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Errorf("paginationParams(%q): expected (%d,%d), got (%d,%d)", tc.query, tc.wantLimit, tc.wantOffset, limit, offset)
			}
		})
	}
}
