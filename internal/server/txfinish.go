// internal/server/txfinish.go
// Request transaction finalization shared by TenantMiddleware,
// CompanyOnlyMiddleware and PlatformTxMiddleware — spec
// 2026-10-06-request-tx-finalization-design §3.
package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

const (
	// handlerSavepoint is taken right after the request transaction opens;
	// RollbackHandlerWrites rolls back to it.
	handlerSavepoint = "nexqia_handler"
	// txKeepOnErrorKey marks writes that must persist even though the
	// response is >= 400 (spec §4).
	txKeepOnErrorKey = "db_tx_keep_on_error"
)

// RunRequestTx runs the rest of the handler chain inside tx, then commits or
// rolls back (spec §3.1 decision table). The response is held in memory
// until that decision, so a client never receives a success status for a
// transaction that failed to commit. The caller has already opened tx and
// set the session GUCs; RunRequestTx owns tx from here on.
func RunRequestTx(c *gin.Context, tx pgx.Tx) {
	// Commit/rollback must not fail only because the client went away.
	finishCtx := context.WithoutCancel(c.Request.Context())
	if _, err := tx.Exec(c.Request.Context(), "SAVEPOINT "+handlerSavepoint); err != nil {
		_ = tx.Rollback(finishCtx)
		logInternalError(c, err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
		return
	}
	c.Set(txContextKey, tx)

	original := c.Writer
	headerSnapshot := original.Header().Clone()
	buffered := &bufferedWriter{ResponseWriter: original}
	c.Writer = buffered
	defer func() {
		if rec := recover(); rec != nil {
			// Never leave the tx (its pool connection and row locks) open on
			// panic; Recovery writes the 500 through the original writer.
			_ = tx.Rollback(finishCtx)
			c.Writer = original
			panic(rec)
		}
	}()

	c.Next()

	c.Writer = original
	status := buffered.Status()
	switch {
	case c.IsAborted() || len(c.Errors) > 0:
		_ = tx.Rollback(finishCtx)
	case status >= http.StatusBadRequest && !c.GetBool(txKeepOnErrorKey):
		_ = tx.Rollback(finishCtx)
	case status >= http.StatusBadRequest:
		if err := tx.Commit(finishCtx); err != nil {
			logInternalError(c, err)
		}
	default:
		if err := tx.Commit(finishCtx); err != nil {
			logInternalError(c, err)
			restoreHeader(original.Header(), headerSnapshot)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to commit transaction"})
			return
		}
	}
	buffered.flushTo(original)
}

// KeepTxOnError marks the writes made so far as required even though the
// response will be >= 400 (spec §4 closed list).
func KeepTxOnError(c *gin.Context) {
	c.Set(txKeepOnErrorKey, true)
}

// RollbackHandlerWrites drops everything written since the request
// transaction opened, then keeps whatever the caller writes next — used by
// middleware that records a trail after the handler (AccessLog, spec §4 #1).
func RollbackHandlerWrites(c *gin.Context) error {
	if _, err := TxFromContext(c).Exec(c.Request.Context(), "ROLLBACK TO SAVEPOINT "+handlerSavepoint); err != nil {
		return err
	}
	KeepTxOnError(c)
	return nil
}

func restoreHeader(h, snapshot http.Header) {
	for k := range h {
		delete(h, k)
	}
	for k, v := range snapshot {
		h[k] = v
	}
}

// bufferedWriter holds status and body in memory (spec §3.2). Header() is the
// original writer's map, so headers set by earlier middleware and by the
// handler go out when flushTo runs.
type bufferedWriter struct {
	gin.ResponseWriter
	status    int
	statusSet bool
	written   bool
	body      bytes.Buffer
}

func (w *bufferedWriter) WriteHeader(code int) {
	if code <= 0 || w.written {
		return
	}
	w.status = code
	w.statusSet = true
}

func (w *bufferedWriter) WriteHeaderNow() {
	w.written = true
}

func (w *bufferedWriter) Write(data []byte) (int, error) {
	w.written = true
	return w.body.Write(data)
}

func (w *bufferedWriter) WriteString(s string) (int, error) {
	w.written = true
	return w.body.WriteString(s)
}

func (w *bufferedWriter) Status() int {
	if w.statusSet {
		return w.status
	}
	return http.StatusOK
}

func (w *bufferedWriter) Size() int {
	if !w.written {
		return -1
	}
	return w.body.Len()
}

func (w *bufferedWriter) Written() bool {
	return w.written
}

// Flush is a no-op: nothing may reach the client before the commit decision.
func (w *bufferedWriter) Flush() {}

func (w *bufferedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack not supported inside request transaction")
}

// flushTo sends the buffered status and body through dst. gin writes the
// final header itself (writermem.WriteHeaderNow after the chain), so a
// status without a body still reaches the client.
func (w *bufferedWriter) flushTo(dst gin.ResponseWriter) {
	if w.statusSet {
		dst.WriteHeader(w.status)
	}
	if w.body.Len() > 0 {
		_, _ = dst.Write(w.body.Bytes())
	} else if w.written {
		dst.WriteHeaderNow()
	}
}
