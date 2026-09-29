// internal/server/errors.go
// Fixed 500 responses (audit S3, spec 2026-09-29-small-security-fixes §4): the real
// error goes to the server log with the ROUTE PATTERN only (c.FullPath(), never the
// URL/query string — no patient data), the client gets a fixed message.
package server

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

const internalErrorMessage = "internal server error"

func logInternalError(c *gin.Context, err error) {
	log.Printf("internal error: %s %s: %v", c.Request.Method, c.FullPath(), err)
}

// respondInternalError replaces c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()}).
func respondInternalError(c *gin.Context, err error) {
	logInternalError(c, err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": internalErrorMessage})
}

// abortInternalError replaces c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
// — keeps the abort so the request transaction still rolls back.
func abortInternalError(c *gin.Context, err error) {
	logInternalError(c, err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": internalErrorMessage})
}
