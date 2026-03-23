# Run unit tests (no external services or API keys required)
test *flags:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ "{{ flags }}" == *"--include-integration"* ]]; then
        echo "Running all tests (unit + integration)..."
        go test -tags=integration -race -count=1 ./...
    else
        echo "Running unit tests..."
        go test -race -count=1 ./...
    fi

# Build the project
build:
    go build ./...

# Run the server
run:
    go run cmd/server/main.go

# Run the server with dev config (local Ollama)
run-dev:
    CONFIG_FILE=config/config.dev.yaml go run cmd/server/main.go

# Lint and fix
lint:
    golangci-lint fmt
    golangci-lint run --fix

# Generate sqlc
sqlc:
    sqlc generate

# Check for dead code
deadcode:
    @echo "=== Checking for transitively dead functions ==="
    @go run golang.org/x/tools/cmd/deadcode@latest -test ./...
