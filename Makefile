.PHONY: migrate-up migrate-down test test-integration lint fmt run sqlc-generate

migrate-up:
	migrate -path migrations -database "$$DATABASE_URL" up

migrate-down:
	migrate -path migrations -database "$$DATABASE_URL" down 1

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

sqlc-generate:
	go tool sqlc generate
