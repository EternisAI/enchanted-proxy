FROM golang:1.24-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o oauth-proxy .

FROM alpine:latest

RUN apk --no-cache add ca-certificates

# create non-root user
RUN addgroup -S appgroup -g 1001 && \
    adduser  -S appuser  -u 1001 -G appgroup

WORKDIR /app
COPY --from=builder /app/oauth-proxy .

USER appuser
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

CMD ["./oauth-proxy"]