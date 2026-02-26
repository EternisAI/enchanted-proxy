# TEE Architecture

This document describes the internal architecture of the enchanted-proxy when running inside an AWS Nitro Enclave. If you're debugging networking issues, unexpected connection failures, or DNS resolution problems, this is the place to look.

## Overview

The enclave runs 5 processes managed by [procfusion](https://github.com/linkdd/procfusion) (a Rust process supervisor). All processes start in parallel, but the Go server waits for Envoy to be ready before accepting traffic.

```
┌─────────────────────────────────────────────────────────┐
│                   AWS Nitro Enclave                      │
│                                                          │
│  procfusion (PID 1)                                      │
│  ├── dnsmasq          DNS (hosts.enclave + localhost)    │
│  ├── envoy            Transparent proxy + TLS ingress    │
│  ├── attestation-proxy  Nitro attestation docs (:9901)   │
│  ├── node_exporter    Prometheus metrics (:9100)         │
│  └── server.sh → server  Go application (:8080, :8081)  │
│                                                          │
└─────────────────────────────────────────────────────────┘
```

Config: [`deploy/procfusion.toml`](../deploy/procfusion.toml)

## Startup Sequence

The Go server does not start immediately. [`deploy/server.sh`](../deploy/server.sh) runs this sequence:

1. Wait for Envoy's status port (`127.0.0.1:9101`) to respond
2. Set default route: `ip route add local default dev lo` (routes all egress through Envoy's transparent proxy)
3. Launch the `server` binary

This ensures all outbound traffic from the Go server is routed through Envoy → Odyn, which enforces the egress allowlist.

## Networking

### Ingress

External traffic enters the enclave through ports defined in [`deploy/enclaver.yaml`](../deploy/enclaver.yaml) `ingress` section. The Enclaver wrapper (Odyn) forwards these to Envoy listeners inside the enclave.

| External Port | Envoy Listener | Destination | Purpose |
|--------------|----------------|-------------|---------|
| 8180 | `ingress_proxy_api` | `127.0.0.1:8080` (Go server) | Main API |
| 8181 | `ingress_telegram_api` | `127.0.0.1:8081` (GraphQL) | Telegram |
| 9100 | passthrough | `127.0.0.1:9100` (node_exporter) | Metrics |
| 9101 | `status` | Envoy admin | Health/status |

Both API ingress listeners (8180, 8181) perform:
- TLS termination (certs from `/var/run/secrets/tls/`)
- Gzip compression
- Inline attestation via `ext_authz` to the attestation-proxy (`127.0.0.1:9901`)
- WebSocket upgrade support

The `/-/attestation` path is routed directly to the attestation-proxy, bypassing the Go server.

Config: [`deploy/envoy.yaml`](../deploy/envoy.yaml) (listeners section)

### Egress (Allowlist)

All outbound traffic from the enclave is filtered. The Go server's traffic flows through:

```
Go server → localhost (Envoy transparent proxy) → Odyn egress (:10000) → Internet
```

Odyn enforces a domain allowlist defined in [`deploy/enclaver.yaml`](../deploy/enclaver.yaml) under `egress.allow`. Only connections to listed domains/IPs are permitted. Everything else is blocked.

The allowlist is grouped by purpose:

| Category | Examples |
|----------|----------|
| AI providers | `api.openai.com`, `openrouter.ai`, `cloud-api.near.ai`, `inference.tinfoil.sh` |
| Payments | `api.stripe.com`, `api.storekit.itunes.apple.com` |
| OAuth | `oauth2.googleapis.com`, `api.twitter.com`, `slack.com` |
| Database | Supabase IPs (hardcoded), `firestore.googleapis.com` |
| Internal services | NATS IPs, Zcash backend, deep research, Ghost Agent |
| Messaging | `api.telegram.org`, `fcm.googleapis.com` |
| Other | `api.linear.app` (problem reports), `serpapi.com`, `api.exa.ai` |

**To add a new external dependency**: Add its domain to `egress.allow` in `deploy/enclaver.yaml` and redeploy. If connecting to it by IP, add the IP directly.

### Envoy Transparent Proxy Listeners

In addition to the HTTPS egress on port 443, Envoy has protocol-specific transparent proxy listeners for non-HTTPS traffic:

| Port | Protocol | Purpose |
|------|----------|---------|
| 443 | HTTPS | General TLS egress (SNI-based routing) |
| 4222 | TCP | NATS messaging |
| 5432 | TCP | PostgreSQL |
| 8000 | TCP | Internal inference hardware (temporary) |
| 20002 | HTTP | Zcash backend (with retry policy) |

Some services use Envoy internal listeners for special TLS handling:
- `internal_near-ai` — HTTP/2 + ALPN negotiation + TLS 1.3 required
- `internal_zcash_backend` — Custom CA cert (`/etc/ssl/certs/eternis/llm-ca.pem`)

Config: [`deploy/envoy.yaml`](../deploy/envoy.yaml) (clusters section)

### DNS

DNS inside the enclave is handled by dnsmasq, configured to:
- Resolve hosts from [`deploy/hosts`](../deploy/hosts) (hardcoded IPs for Supabase and NATS)
- Fall back to `127.0.0.1` for all other lookups (effectively blocking external DNS)

The hosts file maps database hostnames directly to IPs to avoid DNS lookups that would need to leave the enclave. If database IPs change, this file must be updated.

## Docker Build

[`cmd/server/Dockerfile.enclave`](../cmd/server/Dockerfile.enclave) is a multi-stage build assembling all enclave components:

| Stage | Source | Produces |
|-------|--------|----------|
| `builder` | `golang:1.25.0-alpine` | Go server binary |
| `node_exporter` | `quay.io/prometheus/node-exporter:v1.9.1` | Metrics collector |
| `procfusion` | `rust:1.89.0-alpine` (builds from git SHA) | Process supervisor |
| `attestation_proxy` | `ghcr.io/eternisai/attestation-proxy` | Attestation handler |
| Runtime | `envoyproxy/envoy:v1.35.1` | Final image with all components |

The runtime stage installs `ca-certificates`, `dnsmasq`, `iproute2`, and `netcat-openbsd`, then copies all binaries and config files into place. The entrypoint is procfusion.

All base images are pinned to SHA256 digests.

## CI/CD and Attestation

The build pipeline (`.github/workflows/ci-server.yaml`) does:

1. Build the app image from `Dockerfile.enclave`
2. Sign it with [cosign](https://github.com/sigstore/cosign) using GitHub Actions OIDC (keyless)
3. Run `enclaver build` to produce the enclave image with PCR measurements
4. Attach PCR0/PCR1/PCR2 as cosign annotations on the image
5. Upload `measurements.json` to `s3://provenance.eternis.ai`
6. Trigger deployment via gitops

PCR measurements are the cryptographic fingerprint of the enclave contents. Users can verify them against the attestation document returned by `GET /-/attestation`. See [attestation.md](attestation.md) for the full verification walkthrough.

## Key Files Reference

| File | Purpose |
|------|---------|
| `deploy/enclaver.yaml` | Enclave config: ingress ports, egress allowlist, env vars, mounted files |
| `deploy/envoy.yaml` | Envoy proxy: TLS termination, transparent egress, attestation integration |
| `deploy/procfusion.toml` | Process supervisor: which processes run and their commands |
| `deploy/server.sh` | Startup script: waits for Envoy, sets up routing, launches Go server |
| `deploy/hosts` | Static DNS: hardcoded IPs for databases and internal services |
| `cmd/server/Dockerfile.enclave` | Multi-stage Docker build assembling all enclave components |
| `.github/workflows/ci-server.yaml` | CI pipeline: build, sign, measure, deploy |
| `docs/attestation.md` | User-facing verification instructions |

## Common Issues

**Connection refused / timeout to external service**: The domain is probably not in the egress allowlist. Add it to `deploy/enclaver.yaml` `egress.allow` and redeploy.

**DNS resolution failure**: Check `deploy/hosts` for hardcoded entries. External DNS is blocked inside the enclave — all resolution goes through dnsmasq with localhost fallback.

**Database connection failure after IP change**: Supabase and NATS IPs are hardcoded in `deploy/hosts`. If the provider rotates IPs, update the hosts file and redeploy.

**TLS handshake failure to internal service**: Some services use a custom CA (`/etc/ssl/certs/eternis/llm-ca.pem`). Check the Envoy cluster config for the target service in `deploy/envoy.yaml`.

**Server not starting**: The Go server waits for Envoy (port 9101) before launching. If Envoy fails to start, the server will wait indefinitely. Check Envoy logs for config errors.
