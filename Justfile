# Run tests. Override suite to run different test sets:
#   just test                              → unit tests (default, no external deps)
#   just test suite=integration            → all integration tests (needs env vars)
#   just test suite=anonymizer             → anonymizer summary table (for prompt tuning)
#   just test suite=anonymizer filter=email → anonymizer subtests matching "email"
#   just test suite=all                    → unit + integration
suite := "unit"
filter := ""
test:
    #!/usr/bin/env bash
    set -euo pipefail
    case "{{ suite }}" in
        unit)
            echo "Running unit tests..."
            go test -race -count=1 ./...
            ;;
        integration)
            echo "Running all integration tests..."
            go test -tags=integration -v -race -count=1 -timeout 600s ./...
            ;;
        anonymizer)
            if [[ -n "{{ filter }}" ]]; then
                echo "Running anonymizer tests matching '{{ filter }}'..."
                go test -tags=integration -v -count=1 -timeout 300s -run "TestComprehensive_Anonymizer/.*{{ filter }}" ./internal/anonymizer/
            else
                echo "Running anonymizer summary..."
                go test -tags=integration -v -count=1 -timeout 300s -run TestComprehensive_Summary ./internal/anonymizer/
            fi
            ;;
        all)
            echo "Running all tests (unit + integration)..."
            go test -tags=integration -race -count=1 -timeout 600s ./...
            ;;
        *)
            echo "Unknown suite '{{ suite }}'. Options: unit, integration, anonymizer, all"
            exit 1
            ;;
    esac

# Build the project
build:
    go build ./...

# Run the server (optionally filter logs, e.g. just run logs=token)
logs := ""
run:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -n "{{ logs }}" ]]; then
        go run cmd/server/main.go 2>&1 | grep --line-buffered -F -i '[{{ logs }}]'
    else
        go run cmd/server/main.go
    fi



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
