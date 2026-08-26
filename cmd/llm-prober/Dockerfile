# Build stage
FROM golang:1.26.1-alpine3.23@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder

# Set working directory
WORKDIR /app

# Install dependencies needed for building
RUN apk add --no-cache ca-certificates tzdata

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd ./cmd
COPY internal ./internal

# Build the llm-prober binary
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o llm-prober ./cmd/llm-prober

# Runtime stage
FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659
# BusyBox adduser -G sets the primary GID but also appends the user to the
# group's member list, causing a duplicate entry in "id" output. The sed
# clears the redundant supplementary member from /etc/group.
RUN apk add --no-cache ca-certificates && \
    addgroup -S prober && adduser -S -G prober prober && \
    sed -i "s/^\(prober:x:[0-9]*\):.*$/\1:/" /etc/group

COPY --chown=prober:prober config/config.yaml /etc/enchanted-proxy/config.yaml
ENV CONFIG_FILE=/etc/enchanted-proxy/config.yaml

# Probe state lives here, so an endpoint that was failing before a restart still
# announces its recovery. Mount a volume at this path: a restarted container in
# Kubernetes is a new container built from the image, so a database written to
# the container's own filesystem is discarded on every restart and the prober
# would always start fresh. If the directory is unwritable the prober logs a
# warning and runs without persistence rather than failing to start.
# See docs/llm-prober-state.md for the volume requirements.
RUN mkdir -p /var/lib/llm-prober && chown prober:prober /var/lib/llm-prober
ENV LLM_PROBER_STATE_DB=/var/lib/llm-prober/state.db

COPY --chown=prober:prober --from=builder /app/llm-prober /usr/local/bin/llm-prober
EXPOSE 9090
USER prober
CMD ["/usr/local/bin/llm-prober"]
