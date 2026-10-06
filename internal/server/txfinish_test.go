//go:build integration

package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// txProbeRouter builds a minimal router whose middleware opens a transaction
// and hands it to server.RunRequestTx — the same call the production tx
// middlewares make (spec 2026-10-06-request-tx-finalization §3).
func txProbeRouter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE public.tx_probe (id int PRIMARY KEY, note text NOT NULL)`,
		`CREATE TABLE public.tx_probe_child (id int PRIMARY KEY, parent_id int NOT NULL REFERENCES public.tx_probe(id) DEFERRABLE INITIALLY DEFERRED)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(server.Recovery(io.Discard))
	r.Use(func(c *gin.Context) {
		tx, err := pool.Begin(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}
		server.RunRequestTx(c, tx)
	})

	insert := func(c *gin.Context, id int) bool {
		if _, err := server.TxFromContext(c).Exec(c.Request.Context(), "INSERT INTO public.tx_probe (id, note) VALUES ($1, 'probe')", id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return false
		}
		return true
	}
	r.POST("/created", func(c *gin.Context) {
		if insert(c, 1) {
			c.Header("Location", "/probe/1")
			c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": 1}})
		}
	})
	r.POST("/conflict", func(c *gin.Context) {
		if insert(c, 2) {
			c.JSON(http.StatusConflict, gin.H{"error": "conflict"})
		}
	})
	r.POST("/keep", func(c *gin.Context) {
		if insert(c, 3) {
			server.KeepTxOnError(c)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		}
	})
	r.POST("/partial", func(c *gin.Context) {
		if !insert(c, 4) {
			return
		}
		if err := server.RollbackHandlerWrites(c); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if insert(c, 5) {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		}
	})
	r.POST("/no_content", func(c *gin.Context) {
		if insert(c, 6) {
			c.Status(http.StatusNoContent)
		}
	})
	r.POST("/commit_fail", func(c *gin.Context) {
		// parent 999 does not exist; the FK is DEFERRABLE INITIALLY DEFERRED,
		// so the INSERT succeeds and COMMIT fails.
		if _, err := server.TxFromContext(c).Exec(c.Request.Context(), "INSERT INTO public.tx_probe_child (id, parent_id) VALUES (1, 999)"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Header("Location", "/probe-child/1")
		c.JSON(http.StatusOK, gin.H{"data": "ok"})
	})
	r.POST("/panic", func(c *gin.Context) {
		if insert(c, 7) {
			panic("boom")
		}
	})
	r.POST("/abort", func(c *gin.Context) {
		if insert(c, 8) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		}
	})
	r.GET("/csv", func(c *gin.Context) {
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="probe.csv"`)
		c.Status(http.StatusOK)
		_, _ = c.Writer.WriteString("a,b\n1,2\n")
	})
	return r
}

func probeExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM public.tx_probe WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("query tx_probe: %v", err)
	}
	return exists
}

func serveProbe(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestRunRequestTx — decision table of spec §3.1 and writer contract §3.2.
// One container for all cases; each case uses its own probe id.
func TestRunRequestTx(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	r := txProbeRouter(t, ctx, pool)

	t.Run("created commits and keeps status, header and body", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/created")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Location"); got != "/probe/1" {
			t.Fatalf("Location = %q, want /probe/1", got)
		}
		if got := rec.Body.String(); got != `{"data":{"id":1}}` {
			t.Fatalf("body = %s", got)
		}
		if !probeExists(t, ctx, pool, 1) {
			t.Fatal("row 1 not committed")
		}
	})

	t.Run("4xx rolls back but still sends the response", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/conflict")
		if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"conflict"}` {
			t.Fatalf("got %d %s, want 409 conflict body", rec.Code, rec.Body.String())
		}
		if probeExists(t, ctx, pool, 2) {
			t.Fatal("row 2 must be rolled back on 409")
		}
	})

	t.Run("KeepTxOnError commits despite 401", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/keep")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if !probeExists(t, ctx, pool, 3) {
			t.Fatal("row 3 must persist when KeepTxOnError is set")
		}
	})

	t.Run("RollbackHandlerWrites drops earlier writes and keeps later ones", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/partial")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if probeExists(t, ctx, pool, 4) {
			t.Fatal("row 4 (before RollbackHandlerWrites) must be gone")
		}
		if !probeExists(t, ctx, pool, 5) {
			t.Fatal("row 5 (after RollbackHandlerWrites) must persist")
		}
	})

	t.Run("204 without body keeps its status", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/no_content")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("got %d with %d body bytes, want 204 and empty body", rec.Code, rec.Body.Len())
		}
		if !probeExists(t, ctx, pool, 6) {
			t.Fatal("row 6 not committed")
		}
	})

	t.Run("commit failure becomes 500 without handler headers", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/commit_fail")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != `{"error":"failed to commit transaction"}` {
			t.Fatalf("body = %s", got)
		}
		if got := rec.Header().Get("Location"); got != "" {
			t.Fatalf("Location = %q, handler headers must be dropped on commit failure", got)
		}
	})

	t.Run("panic rolls back and releases the connection", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/panic")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if probeExists(t, ctx, pool, 7) {
			t.Fatal("row 7 must be rolled back on panic")
		}
		if n := pool.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("AcquiredConns = %d after panic, want 0 (connection leaked)", n)
		}
	})

	t.Run("abort keeps rolling back", func(t *testing.T) {
		rec := serveProbe(r, http.MethodPost, "/abort")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if probeExists(t, ctx, pool, 8) {
			t.Fatal("row 8 must be rolled back on abort")
		}
	})

	t.Run("csv download keeps headers and body", func(t *testing.T) {
		rec := serveProbe(r, http.MethodGet, "/csv")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="probe.csv"` {
			t.Fatalf("Content-Disposition = %q", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := rec.Body.String(); got != "a,b\n1,2\n" {
			t.Fatalf("body = %q", got)
		}
	})
}
