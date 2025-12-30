# Zcash Integration Overview

A three-tier payment system enabling Zcash (ZEC) cryptocurrency payments for Silo subscriptions using privacy-preserving Orchard shielded transactions.

## Architecture

```
iOS App  ──►  Enchanted Proxy (Go)  ──►  Zcash Backend (Rust)
                     │                         │
               • Auth validation         • View-only wallet
               • Price fetching          • Blockchain sync
                 (Kraken API)            • Payment detection
               • Entitlement updates     • Orchard addresses
                     │                         │
                     ▼                         ▼
                 PostgreSQL              lightwalletd gRPC
                                               │
                                               ▼
                                        Zcash Blockchain
```

## Components

### iOS App

| File | Purpose |
|------|---------|
| `StoreLib/.../ZCashAPIClient.swift` | HTTP client for proxy endpoints |
| `Chats/.../ZCashPayment/ZCashPaymentViewModel.swift` | State machine + payment polling |
| `Chats/.../ZCashPayment/ZCashPaymentScreen.swift` | SwiftUI views |
| `StoreLib/StoreLibDependencies.swift` | Factory DI registration |

**State Flow:**
```
loading → selectProduct → showingInvoice → waitingForPayment → confirming → success/error
```

### Enchanted Proxy (Go)

| File | Purpose |
|------|---------|
| `internal/zcash/handler.go` | HTTP routes (`/api/v1/zcash/*`) |
| `internal/zcash/service.go` | Business logic, Kraken API, Postgres entitlements |

**Endpoints:**
- `GET /api/v1/zcash/products` - List available products
- `POST /api/v1/zcash/invoice` - Create invoice with payment address
- `GET /api/v1/zcash/invoice/:id` - Check payment status
- `POST /api/v1/zcash/confirm` - Verify payment and activate subscription

### Zcash Backend (Rust)

| File | Purpose |
|------|---------|
| `zcash-payment-backend/src/main.rs` | Wallet, sync loop, invoice DB, HTTP API |
| `zcash-payment-backend/Cargo.toml` | Dependencies (zcash stack, axum, sqlite) |

**Key Features:**
- View-only wallet using Unified Full Viewing Key (UFVK)
- Orchard-only address generation (maximum privacy)
- Background sync loop (polls lightwalletd every 15s)
- SQLite invoice database

## Payment Flow

1. **User selects product** → iOS fetches `GET /products`
2. **Create invoice** → Proxy fetches live ZEC/USD from Kraken, calculates amount, forwards to Rust
3. **Rust generates unique Orchard address** → Returns to iOS with ZEC amount
4. **User pays externally** (Zashi wallet, etc.)
5. **iOS polls every 5s** → Proxy forwards to Rust backend
6. **Rust sync loop detects payment** → Marks invoice as paid
7. **iOS detects paid status** → Calls `POST /confirm`
8. **Proxy activates subscription** → `UpsertEntitlementWithTier()` in Postgres

## Products

| ID | Price | Tier | Duration |
|----|-------|------|----------|
| `monthly_pro` | $20 USD | Pro | 30 days |
| `lifetime_plus` | $500 USD | Plus | Forever (2099-12-31) |

Non-production prices are 1% for testing ($0.20 and $5.00).

## Security Design

### View-Only Wallet
The Rust backend only has the viewing key (UFVK), not the spending key. It can detect incoming payments but cannot spend funds. Actual ZEC goes to a separate spending wallet managed offline.

### Orchard Privacy
Uses Orchard shielded protocol exclusively:
- `ReceiverRequirement::Require` for Orchard
- `ReceiverRequirement::Omit` for Sapling and Transparent

Transactions are fully shielded - amounts and addresses invisible on public blockchain.

### Server-Side Pricing
ZEC/USD price fetched from Kraken API on the proxy server, not the client. Prevents price manipulation attacks.

### Invoice Ownership
Invoice ID format embeds user identity: `{userID}_{productID}_{timestamp}`

The confirm endpoint verifies the authenticated user matches the invoice's embedded userID.

### Authentication
All proxy endpoints require valid Firebase ID token. User identity verified before any payment processing.

## Payment Detection

The Rust backend runs a background sync loop every 15 seconds:

1. Connects to lightwalletd, fetches latest blocks
2. Decrypts Orchard notes using the viewing key
3. For each unpaid invoice, queries wallet.db:

```sql
SELECT txid, mined_height, value
FROM addresses a
JOIN orchard_received_notes orn ON orn.address_id = a.id
JOIN transactions t ON t.id_tx = orn.transaction_id
WHERE a.address = ?
  AND orn.is_change = 0
  AND t.mined_height IS NOT NULL
ORDER BY t.mined_height DESC
LIMIT 1
```

4. If payment found, marks invoice as paid with txid, height, and zat amount

## Data Models

### iOS (ZCashAPIClient.swift)
- `ZCashProduct` - id, name, description, priceUSD, tier, isLifetime
- `ZCashInvoice` - invoiceID, address, productID, priceUSD, zecAmount, zatAmount
- `ZCashInvoiceStatus` - invoiceID, address, expectedZat, status, paidZat, paidTxid, paidHeight

### Proxy (service.go)
- `Product` - ID, Name, Description, PriceUSD, Tier, IsLifetime
- `InvoiceStatusResponse` - mirrors Rust backend response

### Rust (main.rs)
- `CreateInvoiceRequest` - invoice_id, expected_zat
- `CreateInvoiceResponse` - invoice_id, address
- `InvoiceStatusResponse` - invoice_id, address, expected_zat, status, paid_zat, paid_txid, paid_height

## Configuration

### Proxy Environment Variables
- `ZCASH_BACKEND_URL` - Rust backend address (default: `http://54.210.176.154:8080`)

### Rust Backend Environment Variables
- `UFVK` - Unified Full Viewing Key (required)
- `LWD_URL` - lightwalletd gRPC endpoint
- `ZCASH_NETWORK` - `"mainnet"` or `"testnet"`
- `HTTP_BIND` - Listen address (default: `0.0.0.0:8080`)
- `WALLET_DB_PATH` - Path to wallet.db
- `INVOICE_DB_PATH` - Path to invoices.db
- `POLL_SECONDS` - Sync interval (default: 15)

## Key Design Decisions

1. **Three-Tier Separation** - iOS handles UI, Go handles auth/pricing/entitlements, Rust handles wallet/blockchain
2. **Polling Architecture** - Simple HTTP polling (iOS 5s, Rust 15s) since webhooks aren't supported
3. **Fresh Addresses Per Invoice** - Each purchase gets unique diversified address for privacy
4. **Zatoshi Precision** - Amounts stored as `i64` zatoshis (1 ZEC = 100M zat) to avoid floating-point issues
5. **Zero-Knowledge Confirmation** - Proxy verifies invoice format and ownership before activating subscription
