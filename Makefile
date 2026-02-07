run:
	go run cmd/server/main.go

run-dev:
	CONFIG_FILE=config/config.dev.yaml go run cmd/server/main.go

lint:
	golangci-lint fmt
	golangci-lint run --fix

build:
	go build ./...

test:
	go test ./... -race

sqlc:
	sqlc generate

deadcode:
	@echo "=== Checking for transitively dead functions ==="
	@go run golang.org/x/tools/cmd/deadcode@latest -test ./...
