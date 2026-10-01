// One-shot: bootstrap the platform.* RBAC catalog + the first platform admin.
//
// Every step is idempotent (ON CONFLICT DO NOTHING/DO UPDATE) — safe to re-run
// in any environment, including production, where this MUST be run once after
// migrate-up (unlike `make seed`, which is demo-only and typically skipped in
// production). See 2026-09-24-platform-admin-foundation-design.md §7.
//
// Reads PLATFORM_ADMIN_BOOTSTRAP_USERNAME/_EMAIL/_PASSWORD from .env (flag -env,
// default ".env") — never hardcoded, never logged.
//
// Run: go run ./cmd/bootstrap-platform-admin -env .env
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
)

func main() {
	envPath := flag.String("env", ".env", "path to .env file")
	flag.Parse()

	if err := godotenv.Load(*envPath); err != nil {
		fmt.Printf("warning: could not load %s: %v (continuing with process env)\n", *envPath, err)
	}

	databaseURL := mustGetEnv("DATABASE_URL")
	username := mustGetEnv("PLATFORM_ADMIN_BOOTSTRAP_USERNAME")
	email := mustGetEnv("PLATFORM_ADMIN_BOOTSTRAP_EMAIL")
	password := mustGetEnv("PLATFORM_ADMIN_BOOTSTRAP_PASSWORD")

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		fmt.Printf("failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	cfg := config.Load()
	hasher := auth.NewPasswordHasher(
		cfg.PasswordHashMaxConcurrent,
		time.Duration(cfg.PasswordHashQueueTimeoutMS)*time.Millisecond,
		auth.Argon2Params{
			MemoryKiB:   cfg.Argon2MemoryKiB,
			Iterations:  cfg.Argon2Iterations,
			Parallelism: cfg.Argon2Parallelism,
		},
	)
	passwordHash, err := hasher.Hash(ctx, password)
	if err != nil {
		fmt.Printf("failed to hash password: %v\n", err)
		os.Exit(1)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Printf("failed to begin transaction: %v\n", err)
		os.Exit(1)
	}
	defer tx.Rollback(ctx)

	// 1. Permission catalog.
	permCodes := []struct{ code, description, module string }{
		{"platform.company.view", "View companies across all tenants", "platform"},
		{"platform.company.manage", "Suspend/activate/create companies", "platform"},
		{"platform.admin.manage", "Manage platform admin users and roles", "platform"},
		{"platform.tenant_user.impersonate", "Impersonate a tenant user for troubleshooting", "platform"},
	}
	for _, p := range permCodes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO platform.permission (code, description, module) VALUES ($1, $2, $3)
			 ON CONFLICT (code) DO UPDATE SET description = EXCLUDED.description, module = EXCLUDED.module`,
			p.code, p.description, p.module,
		); err != nil {
			fmt.Printf("failed to upsert permission %s: %v\n", p.code, err)
			os.Exit(1)
		}
	}

	// 2. "Super Admin" role.
	var roleID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO platform.role (name, description, is_system) VALUES ('Super Admin', 'Full platform access', true)
		 ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description
		 RETURNING id`,
	).Scan(&roleID); err != nil {
		fmt.Printf("failed to upsert Super Admin role: %v\n", err)
		os.Exit(1)
	}

	// 3. Link role to every permission from step 1.
	for _, p := range permCodes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO platform.role_permission (role_id, permission_id)
			 SELECT $1, id FROM platform.permission WHERE code = $2
			 ON CONFLICT (role_id, permission_id) DO NOTHING`,
			roleID, p.code,
		); err != nil {
			fmt.Printf("failed to link permission %s to Super Admin: %v\n", p.code, err)
			os.Exit(1)
		}
	}

	// 4. The admin_user itself.
	var adminUserID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO platform.admin_user (username, email, full_name, password_hash)
		 VALUES ($1, $2, 'Platform Bootstrap Admin', $3)
		 ON CONFLICT (username) DO UPDATE SET email = EXCLUDED.email
		 RETURNING id`,
		username, email, passwordHash,
	).Scan(&adminUserID); err != nil {
		fmt.Printf("failed to upsert admin_user: %v\n", err)
		os.Exit(1)
	}

	// 5. Link admin_user to Super Admin role.
	if _, err := tx.Exec(ctx,
		`INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)
		 ON CONFLICT (admin_user_id, role_id) DO NOTHING`,
		adminUserID, roleID,
	); err != nil {
		fmt.Printf("failed to link admin_user to Super Admin role: %v\n", err)
		os.Exit(1)
	}

	if err := tx.Commit(ctx); err != nil {
		fmt.Printf("failed to commit: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("bootstrapped platform admin %q (id=%s) with role Super Admin\n", username, adminUserID)
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Printf("missing required env var: %s\n", key)
		os.Exit(1)
	}
	return v
}
