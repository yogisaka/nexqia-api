FROM golang:1.27-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# internal/db/sqlcgen/ is gitignored; sqlc falls back to its pure-Go (wasm)
# parser without cgo, so this works in the alpine builder.
RUN CGO_ENABLED=0 go tool sqlc generate
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/yogisaka/nexqia-api/internal/version.Version=${VERSION}" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
