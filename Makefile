.PHONY: migrate-up migrate-down sync-runtime-password seed import-terminology bootstrap-platform-admin test test-integration lint fmt run dev sqlc-generate swag-generate

migrate-up:
	set -a && . ./.env && set +a && $$(go env GOPATH)/bin/migrate -path migrations -database "$$DATABASE_URL" up

migrate-down:
	set -a && . ./.env && set +a && $$(go env GOPATH)/bin/migrate -path migrations -database "$$DATABASE_URL" down 1

# app_runtime's password can't be set via migration (golang-migrate has no :var
# substitution) — run this once after migrate-up, and again whenever APP_RUNTIME_PASSWORD
# in .env changes, to sync the actual DB role password to it.
sync-runtime-password:
	set -a && . ./.env && set +a && psql "$$DATABASE_URL" -v app_runtime_password="$$APP_RUNTIME_PASSWORD" -f scripts/sync_app_runtime_password.sql

seed:
	set -a && . ./.env && set +a && psql "$$DATABASE_URL" -v nik_key="'$$NIK_ENCRYPTION_KEY'" -f seed/001_core_seed.sql

import-terminology:
	set -a && . ./.env && set +a && go run ./cmd/import-terminology $(ARGS)

bootstrap-platform-admin:
	set -a && . ./.env && set +a && go run ./cmd/bootstrap-platform-admin

test:
	go test ./... -short

test-integration:
	docker build -t nexqia-api-postgres-test:latest .docker/postgres-test
	go test ./... -tags=integration

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

run:
	go run ./cmd/api

dev:
	$$(go env GOPATH)/bin/air

sqlc-generate:
	go tool sqlc generate

swag-generate:
	go tool swag init -g cmd/api/main.go -o docs
