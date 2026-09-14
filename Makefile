.PHONY: migrate-up migrate-down seed test test-integration lint fmt run dev sqlc-generate

migrate-up:
	set -a && . ./.env && set +a && $$(go env GOPATH)/bin/migrate -path migrations -database "$$DATABASE_URL" up

migrate-down:
	set -a && . ./.env && set +a && $$(go env GOPATH)/bin/migrate -path migrations -database "$$DATABASE_URL" down 1

seed:
	set -a && . ./.env && set +a && psql "$$DATABASE_URL" -v nik_key="'$$NIK_ENCRYPTION_KEY'" -f seed/001_core_seed.sql

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
