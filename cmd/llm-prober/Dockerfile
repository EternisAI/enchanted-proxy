# Build stage
FROM golang:1.26.1-alpine3.23@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder

# Set working directory
WORKDIR /app

# Install git and other dependencies needed for building
RUN apk add --no-cache git ca-certificates tzdata

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd ./cmd
COPY internal ./internal

# Build the llm-prober binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o llm-prober cmd/llm-prober/main.go

# Runtime stage
FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659
RUN apk add --no-cache ca-certificates

COPY config/config.yaml /etc/enchanted-proxy/config.yaml
ENV CONFIG_FILE=/etc/enchanted-proxy/config.yaml

COPY --from=builder /app/llm-prober /usr/local/bin/llm-prober
CMD ["/usr/local/bin/llm-prober"]
