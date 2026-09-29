// internal/server/logging.go
package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLogger logs one line per request: timestamp, status, latency, client
// IP, method, and URL path. It never logs the query string, headers, or
// cookies — patient name/NIK arrive via ?q=… (PHI, audit finding T2 in
// docs/20-security-compliance-gap-audit.md).
func RequestLogger(w io.Writer) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		fmt.Fprintf(w, "%s %d %s %s %s %s\n",
			start.Format(time.RFC3339),
			c.Writer.Status(),
			time.Since(start),
			c.ClientIP(),
			c.Request.Method,
			c.Request.URL.Path, // Path only — RawQuery/RequestURI carry PHI search terms.
		)
	}
}

// Recovery recovers panics, writes one line (panic value, method, URL path,
// stack trace) to w — no request dump, headers, query, or body — and returns
// 500. Broken client connections (EPIPE/ECONNRESET) abort without a response,
// like gin's built-in recovery.
func Recovery(w io.Writer) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && isBrokenPipe(err) {
				// Connection is dead; writing a status would fail.
				c.Error(err) //nolint: errcheck
				c.Abort()
				return
			}
			fmt.Fprintf(w, "[RECOVERY] %s %s %s panic: %v\n%s\n",
				time.Now().Format(time.RFC3339),
				c.Request.Method,
				c.Request.URL.Path,
				rec,
				debug.Stack(),
			)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
		}()
		c.Next()
	}
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, http.ErrAbortHandler)
}
