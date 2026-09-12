//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestSharedPreludeMigration_CreatesExtensionsFunctionsAndSchemas(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"nexqia-api-postgres-test:latest",
		postgres.WithDatabase("nexqia_test"),
		postgres.WithUsername("nexqia"),
		postgres.WithPassword("nexqia"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	m, err := migrate.New("file://../../migrations", dsn)
	if err != nil {
		t.Fatalf("failed to init migrate: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close(ctx)

	var extCount int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname IN ('pgcrypto','pg_trgm','pg_stat_statements','pg_partman')`).Scan(&extCount)
	if err != nil {
		t.Fatalf("failed to query extensions: %v", err)
	}
	if extCount != 4 {
		t.Errorf("expected 4 extensions installed, got %d", extCount)
	}

	var funcCount int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('uuid_generate_v7','trg_touch_row','trg_check_tenant_consistency')`).Scan(&funcCount)
	if err != nil {
		t.Fatalf("failed to query functions: %v", err)
	}
	if funcCount != 3 {
		t.Errorf("expected 3 shared functions, got %d", funcCount)
	}

	var schemaCount int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name IN ('core','terminology')`).Scan(&schemaCount)
	if err != nil {
		t.Fatalf("failed to query schemas: %v", err)
	}
	if schemaCount != 2 {
		t.Errorf("expected 2 schemas (core, terminology), got %d", schemaCount)
	}

	var rolsuper, rolbypassrls bool
	err = conn.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'app_runtime'`).Scan(&rolsuper, &rolbypassrls)
	if err != nil {
		t.Fatalf("failed to query app_runtime role: %v", err)
	}
	if rolsuper {
		t.Error("app_runtime must not be superuser")
	}
	if rolbypassrls {
		t.Error("app_runtime must not bypass row level security")
	}
}
