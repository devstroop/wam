.PHONY: run build vet test fmt lint openapi-lint tidy docker-build docker-up clean migrate migrate-schema

APP := wam
PKG := ./...
ADDR ?= :8080

run:
	go run ./cmd/wam

# Import legacy SQLite into Postgres (requires WAM_DATABASE_URL).
# Usage: make migrate [FROM=./data/wam.db] [DATABASE_URL=postgres://...]
migrate:
	go run ./cmd/wam migrate --from-sqlite "$${FROM:-./data/wam.db}" --database-url "$${DATABASE_URL:-$$WAM_DATABASE_URL}"

# Apply pending Postgres schema migrations (use owner/superuser DSN).
migrate-schema:
	go run ./cmd/wam migrate-schema --database-url "$${DATABASE_URL:-$$WAM_DATABASE_URL}"

build:
	go build -o bin/$(APP) ./cmd/wam

vet:
	go vet $(PKG)

test:
	go test $(PKG)

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

# Validate OpenAPI locally if vacuum/spectral/redocly is installed (optional).
openapi-lint:
	@if command -v vacuum >/dev/null 2>&1; then vacuum lint api/openapi.yaml; \
	elif command -v redocly >/dev/null 2>&1; then redocly lint api/openapi.yaml; \
	else echo "no OpenAPI linter found (vacuum/redocly). Skipping."; fi

docker-build:
	docker build -t $(APP):dev .

docker-up:
	docker compose up --build

clean:
	rm -rf bin
