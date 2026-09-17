<p align="center">
  <img src="docs/assets/brand/full-logo.png" alt="NEXQIA" width="360">
</p>

# NEXQIA API

Backend service for the NEXQIA Healthcare Operating Platform, written in Go.

See the [nexqia](https://github.com/yogisaka/nexqia) repository for the full product overview, architecture, and infrastructure plan.

## Tech stack

- **Go 1.27**, [Gin](https://github.com/gin-gonic/gin) HTTP router
- **PostgreSQL** via [pgx/v5](https://github.com/jackc/pgx) — row-level security (RLS) for multi-tenant isolation
- [sqlc](https://sqlc.dev/) — typed Go query layer generated from raw SQL (`internal/db/queries/*.sql` → `internal/db/sqlcgen/*.sql.go`)
- [golang-migrate](https://github.com/golang-migrate/migrate) — versioned SQL migrations
- **Redis** — rate limiting, session/app-lock state
- JWT (access + refresh token) auth, Argon2id password hashing, TOTP 2FA, PIN app-lock

## Project structure

```
cmd/api/            entrypoint (main.go) — loads config, connects DB/Redis, starts router
internal/
  auth/              password hashing (Argon2id), JWT
  cache/             Redis client
  config/            env var loading (internal/config/config.go)
  db/
    queries/          hand-written .sql files, input to sqlc
    sqlcgen/           generated Go query code — never hand-edit
  mfa/               TOTP 2FA (encrypt/decrypt secret, verify code)
  ratelimit/         Redis-backed rate limiter
  server/            HTTP layer — router wiring + all handlers, one file per resource
  session/           refresh token / session model
  version/           build version string
migrations/          golang-migrate up/down SQL pairs, numbered sequentially
seed/                seed SQL (roles, permissions, dev data)
deploy/              production docker-compose + nginx template
.docker/postgres-test/  Postgres image w/ pg_partman, used by integration tests
```

Each `internal/server/*.go` file owns one resource's routes + handlers (e.g. `department.go`, `queue.go`). `server.go`'s `NewRouter` is the single place all `Register*Routes` calls are wired together.

## Database schema

Schemas map to product domains, each with its own DDL design doc in the [`nexqia`](https://github.com/yogisaka/nexqia) repo's `docs/` folder:

| Schema | Purpose | Design doc |
|---|---|---|
| `core` | tenancy (company/merchant), RBAC, person/physician master, department, tariff, payer, audit, notification, auth | `docs/07-core-ddl.md` |
| `operations` | physician schedules, counters, multi-stage queue, admission (kunjungan) | `docs/08-operations-ddl.md` |

Generated Go struct names are prefixed with the PascalCase schema+table (e.g. `operations.physician_schedule` → `sqlcgen.OperationsPhysicianSchedule`), independent of which `.sql`/`.sql.go` file they live in.

Migrations are numbered sequentially in `migrations/` (`000001` ... ) regardless of schema — check the file list, not the DDL docs, to know what's actually been applied.

## Getting started

### Prerequisites

- Go 1.27+
- PostgreSQL 18 (with `pgcrypto` extension — used for `core.person.nik` encryption)
- Redis
- [`golang-migrate`](https://github.com/golang-migrate/migrate) CLI (`go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest`)
- [`air`](https://github.com/air-verse/air) for hot reload in dev (optional — `make dev`)
- `sqlc` is **not** a separate install — it's a Go 1.24+ tool dependency, run via `go tool sqlc generate` (see `Makefile`'s `sqlc-generate` target)

### Setup

```bash
cp .env.example .env
# fill in DB/Redis credentials, JWT_SECRET, NIK_ENCRYPTION_KEY, MFA_SECRET_ENCRYPTION_KEY — see comments in .env.example

make migrate-up
make sync-runtime-password   # syncs app_runtime's DB password to APP_RUNTIME_PASSWORD in .env
make seed
make run       # or `make dev` for hot reload via air
```

Server listens on `HTTP_PORT` (default `8080`), routes under `/api/v1`.

### Makefile targets

| Target | Does |
|---|---|
| `make run` | `go run ./cmd/api` |
| `make dev` | hot-reload dev server via `air` |
| `make migrate-up` / `make migrate-down` | apply / roll back one migration, using `DATABASE_URL` (owner role, not `app_runtime`) |
| `make sync-runtime-password` | sync the `app_runtime` role's DB password to `APP_RUNTIME_PASSWORD` in `.env` — run once after `migrate-up`, and again whenever that value changes |
| `make seed` | run `seed/001_core_seed.sql` (roles, permissions, dev fixtures) |
| `make sqlc-generate` | regenerate `internal/db/sqlcgen` from `internal/db/queries/*.sql` |
| `make test` | `go test ./... -short` |
| `make test-integration` | builds the `.docker/postgres-test` image, runs `go test ./... -tags=integration` (uses testcontainers) |
| `make lint` | `golangci-lint run` |
| `make fmt` | `golangci-lint fmt` |

## Configuration

All runtime config is environment variables, loaded and validated in `internal/config/config.go` (fails fast on startup if a required value is missing/invalid — see `MFASecretEncryptionKey` check in `server.NewRouter`). Full list with explanations lives in `.env.example`; key groups:

- **DB / Redis connection** — `DB_HOST`, `DB_PORT`, `DB_NAME`, `APP_RUNTIME_USER`, `APP_RUNTIME_PASSWORD`, `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`
- **Migrations only** — `DATABASE_URL` (owner role, only used by `make migrate-up`/`migrate-down`/`seed`, never by the running server)
- **Secrets** — `JWT_SECRET`, `NIK_ENCRYPTION_KEY` (pgcrypto symmetric key for `core.person.nik`), `MFA_SECRET_ENCRYPTION_KEY` (AES-256-GCM, base64, for TOTP secrets — deliberately separate from `NIK_ENCRYPTION_KEY` so rotating one doesn't invalidate the other)
- **Auth tuning** — `ACCESS_TOKEN_TTL_MINUTES`, `REFRESH_TOKEN_IDLE_TIMEOUT_HOURS`, `REFRESH_TOKEN_ABSOLUTE_TTL_DAYS`, `COOKIE_SECURE`, Argon2id params (`ARGON2_MEMORY_KIB`/`ARGON2_ITERATIONS`/`ARGON2_PARALLELISM`), password-hash concurrency limits
- **Rate limiting** — `RATE_LIMIT_LOGIN_MAX_ATTEMPTS`/`_WINDOW_SECONDS`, `RATE_LIMIT_API_TOKENS_PER_MINUTE`/`_BURST`
- **PIN app-lock** — `APP_LOCK_DEFAULT_IDLE_MINUTES`, `APP_LOCK_MAX_PIN_ATTEMPTS`, `APP_LOCK_ATTEMPT_WINDOW_MINUTES` (opt-in per merchant via `core.feature_flag "auth.pin_lock"`, these are just fallback/hard limits)

Never commit a real `.env` — only `.env.example`.

## Authentication & authorization

- **Auth flow:** `POST /auth/login` → JWT access token (+ refresh token cookie) → `POST /auth/select-merchant` picks the active merchant (multi-tenant: one company, many merchants) → `POST /auth/switch-merchant` to change without re-login.
- **Middleware chain** (`server.go`): `CompanyOnlyMiddleware` (pre-merchant-selection) → `TenantMiddleware` (opens a per-request DB transaction, sets `app.current_company_id`/`app.current_merchant_id` session vars for RLS) → `AuthMiddleware` (JWT validation) → `RateLimitAPIMiddleware` → `AppLockMiddleware` (PIN re-lock on idle, skipped for `/auth/pin/*` routes so a locked session can still unlock itself).
- **Permissions:** one `Perm<Resource>Manage` constant per resource, checked via `RequirePermission(c, code)` (resolves merchant from the `X-Merchant-ID` header — used for list/create) or `RequirePermissionForMerchant(c, code, merchantID)` (checks against a specific resource's actual `merchant_id` — used for get/update/delete, and for create handlers where `merchant_id` comes from the request body, to prevent a caller from creating data under a merchant they only declared in the header). Every `Perm*` constant must have a matching row in `seed/001_core_seed.sql`'s `core.permission` insert and be granted to a role, or the check always fails silently — not enforced by the compiler.
- **Multi-write handlers** (e.g. admission creation, queue status transitions) call `c.Error(err)` before returning a JSON error on any write after the first, so `TenantMiddleware` rolls back the whole transaction instead of committing a partial write.

## API routes

All routes are under `/api/v1`. Response envelope is `{"data": ..., "meta": {...}}` (list endpoints add `limit`/`offset` to `meta`).

### Pre-auth / session

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | liveness |
| GET | `/version` | build version |
| POST | `/auth/login` | rate-limited |
| POST | `/auth/select-merchant` | issues merchant-scoped token |
| POST | `/auth/refresh` | rotate access token via refresh cookie |
| POST | `/auth/logout` | revoke refresh token |

### Authenticated (require valid JWT; PIN routes stay reachable while app-locked)

| Method | Path | Notes |
|---|---|---|
| POST | `/auth/pin/set` \| `/auth/pin/disable` \| `/auth/pin/verify` | PIN app-lock management |

### Authenticated + unlocked (behind `AppLockMiddleware`)

| Method | Path |
|---|---|
| GET | `/ping` |
| POST | `/auth/switch-merchant` |
| GET | `/auth/sessions` |
| DELETE | `/auth/sessions/:id` |

**Tenancy** — `core.company` / `core.merchant`

| Method | Path |
|---|---|
| POST/GET | `/companies` |
| GET/PATCH/DELETE | `/companies/:id` |
| POST | `/merchants` |
| GET | `/companies/:id/merchants` |
| GET/PATCH/DELETE | `/merchants/:id` |

**RBAC** — `core.app_user` / `core.role` / `core.permission` / `core.user_merchant_role`

| Method | Path |
|---|---|
| POST/GET | `/users` |
| GET/PATCH/DELETE | `/users/:id` |
| PUT | `/users/:id/password` |
| GET | `/permissions` |
| POST/GET | `/roles` |
| GET/PATCH/DELETE | `/roles/:id` |
| GET/POST | `/roles/:id/permissions` |
| DELETE | `/roles/:id/permissions/:permission_id` |
| POST/GET | `/users/:id/merchant-roles` |
| GET | `/users/:id/roles` |
| DELETE | `/merchant-roles/:id` |

**MFA** (TOTP)

| Method | Path |
|---|---|
| POST | `/auth/mfa/setup` \| `/auth/mfa/confirm` \| `/auth/mfa/disable` |

**Core master data** — `core.person` / `core.department` / `core.physician` / tariff engine

| Method | Path |
|---|---|
| POST/GET | `/persons` |
| GET | `/persons/search` |
| GET/PATCH/DELETE | `/persons/:id` |
| POST | `/departments` |
| GET | `/merchants/:id/departments` |
| GET/PATCH/DELETE | `/departments/:id` |
| POST | `/physicians` |
| GET | `/merchants/:id/physicians` |
| GET/PATCH/DELETE | `/physicians/:id` |
| POST/GET | `/service-items`, `/rate-components`, `/service-rates` (+ `/merchants/:id/...`, `/:id`, `/service-items/:id/rates`) |

**Operations** — physician schedule, counter, queue, admission (kunjungan)

| Method | Path |
|---|---|
| POST | `/physician-schedules` |
| GET | `/merchants/:id/physician-schedules` |
| GET/PATCH/DELETE | `/physician-schedules/:id` |
| POST | `/counters` |
| GET | `/merchants/:id/counters` |
| GET/PATCH | `/counters/:id` |
| POST | `/counters/:id/call-next` — pulls oldest waiting queue for that counter, marks `called` |
| GET | `/queue` \| `/queue/:id` |
| PATCH | `/queue/:id` — status transition; `status=done` auto-creates the next pipeline stage's queue row |
| POST/GET | `/admissions` |
| GET/PATCH | `/admissions/:id` |
| GET | `/display/queue` \| `/display/schedule` — read-only, poll-based, for waiting-room boards |

The queue pipeline (`pendaftaran` → `perawat` → `dokter`) is a fixed Go slice in `internal/server/queue.go`, not a DB table — future service lines extend the slice, not the schema.

## Testing

- `make test` — unit tests, `-short` skips integration suites
- `make test-integration` — builds the test Postgres image (`.docker/postgres-test`, includes `pg_partman`) and runs `go test ./... -tags=integration`, using [testcontainers-go](https://golang.testcontainers.org/) to spin up real Postgres/Redis per test

## Deployment

`deploy/docker-compose.yml` + `deploy/nginx.conf.template` describe the production layout: the API container talks to `pgbouncer` and `redis` over an external Docker network (`nexqia_net`), fronted by an nginx reverse proxy. See `deploy/.env.example` for required compose-level env vars, and the [`nexqia`](https://github.com/yogisaka/nexqia) repo for the full branching/deploy flow.

## Further reading

- [`nexqia`](https://github.com/yogisaka/nexqia) repo — product overview, DDL design docs (`docs/07-core-ddl.md`, `docs/08-operations-ddl.md`, ...), infra/branching strategy.
