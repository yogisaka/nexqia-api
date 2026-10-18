package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// expected values computed from contract:
// - required on `full_name` with empty body -> fields.full_name = "Wajib diisi"
// - min=8 on string -> "Minimal 8 karakter"
// - gte=0 with -1 -> "Minimal 0"
// - json string into int field -> "Tipe data tidak valid" keyed by json name
// - malformed body -> {"error":"invalid request body"} without fields
func TestBindJSONOrFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setup := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		return c, w
	}

	t.Run("required", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		var req struct {
			Name string `json:"full_name" binding:"required"`
		}
		if bindJSONOrFields(c, &req) {
			t.Fatal("expected false for missing required field")
		}
		body := w.Body.String()
		if !strings.Contains(body, `"validation_failed"`) {
			t.Fatalf("expected error validation_failed, got %s", body)
		}
		if !strings.Contains(body, `"full_name":"Wajib diisi"`) {
			t.Fatalf("expected fields.full_name=Wajib diisi, got %s", body)
		}
		if w.Code != 400 {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("min_string", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"code":"ab"}`))
		var req struct {
			Code string `json:"code" binding:"min=8"`
		}
		if bindJSONOrFields(c, &req) {
			t.Fatal("expected false for short string")
		}
		if !strings.Contains(w.Body.String(), `"code":"Minimal 8 karakter"`) {
			t.Fatalf("expected Minimal 8 karakter, got %s", w.Body.String())
		}
	})

	t.Run("gte_number", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"qty":-1}`))
		var req struct {
			Qty int `json:"qty" binding:"gte=0"`
		}
		if bindJSONOrFields(c, &req) {
			t.Fatal("expected false for negative number")
		}
		if !strings.Contains(w.Body.String(), `"qty":"Minimal 0"`) {
			t.Fatalf("expected Minimal 0, got %s", w.Body.String())
		}
	})

	t.Run("type_error", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"qty":"abc"}`))
		var req struct {
			Qty int `json:"qty"`
		}
		if bindJSONOrFields(c, &req) {
			t.Fatal("expected false for type error")
		}
		if !strings.Contains(w.Body.String(), `"qty":"Tipe data tidak valid"`) {
			t.Fatalf("expected Tipe data tidak valid on qty, got %s", w.Body.String())
		}
	})

	t.Run("malformed", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{`))
		var req struct {
			Name string `json:"full_name"`
		}
		if bindJSONOrFields(c, &req) {
			t.Fatal("expected false for malformed body")
		}
		body := w.Body.String()
		if !strings.Contains(body, `"invalid request body"`) {
			t.Fatalf("expected invalid request body, got %s", body)
		}
		if strings.Contains(body, `"fields"`) {
			t.Fatalf("expected no fields key, got %s", body)
		}
	})

	t.Run("ok", func(t *testing.T) {
		c, w := setup()
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"ok"}`))
		var req struct {
			Name string `json:"name"`
		}
		if !bindJSONOrFields(c, &req) {
			t.Fatal("expected true for valid body")
		}
		if w.Body.String() != "" {
			t.Fatalf("expected no response body, got %s", w.Body.String())
		}
	})
}
