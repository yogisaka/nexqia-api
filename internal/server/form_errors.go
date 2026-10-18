package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

// bindJSONOrFields binds the request body as JSON. On success it returns true.
// On failure it responds with a per-field error map and returns false. The
// global gin validator is not modified.
func bindJSONOrFields(c *gin.Context, req any) bool {
	err := c.ShouldBindJSON(req)
	if err == nil {
		return true
	}

	var validationErrs validator.ValidationErrors
	if errors.As(err, &validationErrs) {
		fields := map[string]string{}
		for _, fe := range validationErrs {
			field := jsonFieldName(req, fe.StructField())
			if field == "" {
				field = fe.Field()
			}
			fields[field] = validationMessage(fe)
		}
		respondFieldErrors(c, "validation_failed", fields)
		return false
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		respondFieldErrors(c, "validation_failed", map[string]string{
			typeErr.Field: "Tipe data tidak valid",
		})
		return false
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
	return false
}

// respondFieldErrors responds with a 400 carrying a per-field error map.
func respondFieldErrors(c *gin.Context, errMsg string, fields map[string]string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": errMsg, "fields": fields})
}

// fieldErr responds with a single-field error (for existing one-field checks).
func fieldErr(c *gin.Context, errMsg, field, msg string) {
	respondFieldErrors(c, errMsg, map[string]string{field: msg})
}

// jsonFieldName returns the json tag name (before any comma) of the struct
// field with the given name, or "" if not found.
func jsonFieldName(req any, structField string) string {
	t := reflect.TypeOf(req)
	if t == nil {
		return ""
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	f, ok := t.FieldByName(structField)
	if !ok {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

// validationMessage maps a validator tag to a fixed Indonesian message.
func validationMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "Wajib diisi"
	case "min":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("Minimal %s karakter", fe.Param())
		}
		return fmt.Sprintf("Minimal %s", fe.Param())
	case "max":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("Maksimal %s karakter", fe.Param())
		}
		return fmt.Sprintf("Maksimal %s", fe.Param())
	case "gte":
		return fmt.Sprintf("Minimal %s", fe.Param())
	case "lte":
		return fmt.Sprintf("Maksimal %s", fe.Param())
	case "oneof":
		return "Pilihan tidak valid"
	case "email":
		return "Email tidak valid"
	default:
		return "Tidak valid"
	}
}
