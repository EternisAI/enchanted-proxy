run:
	go run cmd/server/main.go

lint:
	golangci-lint fmt
	golangci-lint run --fix

build:
	go build ./...

test:
	go test ./... -race

sqlc:
	sqlc generate
