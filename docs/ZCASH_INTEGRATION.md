# ZCash Payment Integration

## Overview

This document explains how the ZCash integration works across the Silo ecosystem. Users can purchase subscriptions using ZCash (ZEC) cryptocurrency with **shielded (private) Orchard transactions**.

### 3-Tier Architecture

```
iOS App  ←→  Enchanted Proxy (Go)  ←→  ZCash Backend (Rust)
                                              ↓
                                         lightwalletd gRPC
                                              ↓
                                        ZCash Blockchain
```

---

## Components

### 1. iOS App

**Files:**
- [ZCashPaymentScreen.swift](../../private-freysa-ios/Chats/Sources/Chats/Presentation/Pages/ZCashPayment/ZCashPaymentScreen.swift)
- [ZCashPaymentViewModel.swift](../../private-freysa-ios/Chats/Sources/Chats/Presentation/Pages/ZCashPayment/ZCashPaymentViewModel.swift)
- [ZCashAPIClient.swift](../../private-freysa-ios/StoreLib/Sources/StoreLib/Services/API/ZCashAPIClient.swift)

**State Machine:**
```
loading → selectProduct → showingInvoice → waitingForPayment → success/error
```

**Key Features:**
- Displays available products (Pro Monthly $20, Lifetime Plus $500)
- Creates invoices and gets unique payment addresses
- **Polls every 5 seconds** for payment confirmation
- Calls `/confirm` endpoint to activate subscription
- Shows success/error screens with user-friendly messaging

### 2. Enchanted Proxy (Go)

**Files:**
- `internal/zcash/handler.go` - HTTP request handlers
- `internal/zcash/service.go` - Business logic and integrations

**Endpoints:**
- `GET /api/v1/zcash/products` → List available products
- `POST /api/v1/zcash/invoice` → Create invoice
- `GET /api/v1/zcash/invoice/:id` → Check payment status
- `POST /api/v1/zcash/confirm` → Verify payment & activate subscription

**Key Responsibilities:**

1. **Product Management**
   - Pro Monthly: $20 USD, 30-day tier
   - Lifetime Plus: $500 USD, forever tier (expires 2099-12-31)
   - Prices are 1% in non-production for testing

2. **Price Conversion**
   - Fetches live ZEC/USD from **Kraken API** (server-side only)
   - Calculates required ZEC: `product_price_usd / zec_price_usd`
   - Converts to zatoshis (1 ZEC = 100M zat) for precision

3. **Invoice Generation**
   - Calls Rust backend to create invoice and get unique Orchard address
   - Invoice ID format: `{userID}_{productID}_{timestamp}`
   - Example: `abc123_lifetime_plus_1735063823`

4. **Payment Confirmation**
   - Verifies invoice is marked as paid in Rust backend
   - Updates PostgreSQL with `UpsertEntitlementWithTier()`:
     ```go
     UpsertEntitlementWithTier(ctx, {
         UserID:               userID,
         SubscriptionTier:     product.Tier,        // "pro" or "plus"
         SubscriptionExpiresAt: expiresAt,          // 30 days or 2099-12-31
         SubscriptionProvider:  "zcash",
     })
     ```

### 3. ZCash Backend (Rust)

**Files:**
- `zcash-payment-backend/src/main.rs`

**What it does:**
- Runs a **view-only Zcash wallet** using a Unified Full Viewing Key (UFVK)
- Syncs with the Zcash blockchain via lightwalletd gRPC
- Generates **fresh Orchard-only unified addresses** per invoice
- Monitors blockchain for incoming payments
- Marks invoices as "paid" when payment is confirmed

**Two Databases:**

1. **wallet.db** (Zcash)
   - Stores the wallet's Unified Address and diversified addresses
   - Stores decrypted transaction notes from the blockchain
   - Managed by zcash-client-sqlite library

2. **invoice.db** (SQLite)
   - Maps `invoice_id → address → payment_status`
   - Stores `expected_zat` amount for each invoice
   - Updated when payments are detected

**Invoice Lifecycle:**
1. Client calls `POST /invoices {invoice_id, expected_zat}`
2. Backend generates fresh Orchard-only unified address
3. Stores invoice mapping in invoice.db
4. Returns address to client

**Payment Detection:**

The backend runs a background sync loop every 15 seconds:

1. Connects to lightwalletd and fetches latest blocks
2. Scans for new decrypted Orchard notes in wallet.db
3. Queries for unpaid invoices
4. For each unpaid invoice, runs:
   ```sql
   SELECT txid, mined_height, value
   FROM addresses a
   JOIN orchard_received_notes orn ON orn.address_id = a.id
   JOIN transactions t ON t.id_tx = orn.transaction_id
   WHERE a.address = ?              -- Invoice's Orchard address
     AND orn.is_change = 0          -- Not change output
     AND t.mined_height IS NOT NULL -- Confirmed (≥1 block)
   ORDER BY t.mined_height DESC LIMIT 1
   ```
5. If payment found: `mark_paid(invoice_id, txid, height, zat_amount)`

**Orchard-Only Design:**
- Uses `ReceiverRequirement::Require` for Orchard
- Uses `ReceiverRequirement::Omit` for Sapling and Transparent
- Ensures **maximum privacy**: amounts and addresses are shielded

---

## Complete User Journey

### Step-by-Step Flow

1. **Load Products**
   ```
   iOS → GET /api/v1/zcash/products
   ← [{id: "monthly_pro", name: "Pro Monthly", priceUSD: 20}, ...]
   ```

2. **Create Invoice**
   ```
   iOS → POST /api/v1/zcash/invoice {product_id: "lifetime_plus"}
   
   Proxy:
   - Calls Kraken API → ZEC/USD ≈ $30
   - Calculates: $500 / $30 = 16.67 ZEC
   - Converts: 16.67 × 100M = 1,667,000,000 zat
   - Calls Rust backend: POST /invoices {invoice_id: "abc123_lifetime_plus_1735063823", expected_zat: 1667000000}
   
   Rust Backend:
   - Derives fresh unified address: u1...orchard...
   - Stores in invoice.db
   
   ← {invoice_id, address, product_id, price_usd: 500, zec_amount: 16.67, zat_amount: 1667000000}
   ```

3. **Display Invoice**
   ```
   iOS displays:
   - Orchard address to send ZEC to
   - Amount: 16.67 ZEC
   - USD equivalent: $500
   - User can copy address or use context menu
   ```

4. **User Sends Payment**
   ```
   User opens ZCash wallet → Sends 16.67 ZEC to the address
   Transaction broadcasts to ZCash network
   lightwalletd receives and stores the encrypted note
   ```

5. **Frontend Polls for Payment**
   ```
   iOS polls every 5 seconds:
   GET /api/v1/zcash/invoice/:id
   
   Proxy calls Rust backend:
   GET /invoices/:id
   
   Rust responds with status:
   {status: "unpaid"} or {status: "paid", txid: "...", height: ..., paid_zat: ...}
   ```

6. **Backend Detects Payment**
   ```
   Rust sync loop (every 15s):
   - Calls lightwalletd to get latest blocks
   - Decrypts Orchard notes for the viewing key
   - Finds note received at invoice's address
   - Queries wallet.db for the transaction
   - Marks invoice as paid with txid, height, zat_amount
   ```

7. **Frontend Detects Confirmation**
   ```
   Next poll returns {status: "paid"}
   iOS displays "Payment detected, confirming..."
   ```

8. **Activate Subscription**
   ```
   iOS → POST /api/v1/zcash/confirm {invoice_id: "abc123_lifetime_plus_1735063823"}
   
   Proxy:
   - Verifies invoice is marked paid
   - Extracts tier and duration from product
   - Calls: UpsertEntitlementWithTier(
       userID, 
       tier="plus", 
       expiresAt=2099-12-31,
       provider="zcash"
     )
   
   ← {status: "confirmed"}
   ```

9. **Success**
   ```
   iOS shows:
   ✅ Payment Successful!
   Your Lifetime Plus subscription is now active.
   [Done button]
   ```

---

## Key Design Decisions

### 1. Server-Side Price Fetching
- ZEC/USD price comes from Kraken API on the proxy, not the client
- **Why:** Prevents users from manipulating the ZEC amount
- Implementation: `GetZecPriceUSD()` in service.go

### 2. View-Only Wallet
- Backend uses UFVK (Unified Full Viewing Key) only
- Cannot spend funds; only view and detect payments
- **Why:** If backend is compromised, attacker can't steal ZEC
- Funds go to a separate spending wallet (managed offline)

### 3. Orchard-Only Addresses
- Each invoice gets a fresh Orchard-only address
- Sapling and Transparent receivers are omitted
- **Why:** Maximum privacy; shielded transactions hide amounts and addresses

### 4. Amount Stored as Zatoshis
- 1 ZEC = 100,000,000 zat
- Stored as `i64` to avoid floating-point precision issues
- **Why:** Accurate payment matching

### 5. Polling, Not Webhooks
- iOS polls every 5 seconds
- Rust backend polls lightwalletd every 15 seconds
- **Why:** Rust backend doesn't support push notifications; simple HTTP polling works fine for user timelines

### 6. Fresh Addresses Per Invoice
- Each purchase gets a unique diversified address
- **Why:** Better privacy; harder to link multiple purchases to the same wallet

---

## Data Flow Diagram

```
┌──────────────┐         ┌──────────────────┐         ┌────────────────────┐
│   iOS App    │         │  Enchanted Proxy │         │  ZCash Backend     │
│              │         │      (Go)        │         │     (Rust)         │
└──────┬───────┘         └────────┬─────────┘         └──────────┬─────────┘
       │                          │                              │
       │  1. GET /products        │                              │
       │─────────────────────────►│                              │
       │  [products list]         │                              │
       │◄─────────────────────────│                              │
       │                          │                              │
       │  2. POST /invoice        │                              │
       │    {product_id}          │                              │
       │─────────────────────────►│  3. GET Kraken ZEC/USD      │
       │                          ├──────────────────────────────┤
       │                          │  4. POST /invoices           │
       │                          │    {invoice_id, expected_zat}│
       │                          │─────────────────────────────►│
       │                          │  [address, invoice_id]       │
       │                          │◄─────────────────────────────│
       │  [invoice + zec_amount]  │                              │
       │◄─────────────────────────│                              │
       │                          │                              │
       │  === User sends ZEC ===                                 │
       │                          │                              │
       │  5. GET /invoice/:id     │  6. GET /invoices/:id        │
       │   (poll every 5s)        │   (forwards)                 │
       │─────────────────────────►│─────────────────────────────►│
       │  [status: unpaid]        │  [status from blockchain]    │
       │◄─────────────────────────│◄─────────────────────────────│
       │                          │                              │
       │  ... transaction mines ...                              │
       │                          │    Background sync finds     │
       │                          │    payment, marks paid       │
       │                          │                              │
       │  7. GET /invoice/:id     │                              │
       │─────────────────────────►│─────────────────────────────►│
       │  [status: paid]          │  [status: paid]              │
       │◄─────────────────────────│◄─────────────────────────────│
       │                          │                              │
       │  8. POST /confirm        │                              │
       │    {invoice_id}          │                              │
       │─────────────────────────►│  Verify paid, parse invoice  │
       │                          │  UpsertEntitlement(tier,exp) │
       │  [status: confirmed]     │                              │
       │◄─────────────────────────│                              │
       │                          │                              │
       │  ✓ Subscription Active   │                              │
```

---

## Security Properties

1. **Privacy**
   - Shielded Orchard transactions hide amounts and addresses from public blockchain
   - Only sender and receiver can see transaction details

2. **No Spending Key Exposure**
   - Backend only has viewing key (UFVK)
   - Even if compromised, attacker cannot spend ZEC
   - Spending key remains offline

3. **Server-Controlled Pricing**
   - Client cannot manipulate ZEC amount
   - Price always fetched fresh from Kraken

4. **Invoice Ownership Validation**
   - Invoice ID embeds userID: `{userID}_{productID}_{timestamp}`
   - Confirm endpoint verifies userID matches

5. **Firebase Authentication Required**
   - All proxy endpoints require valid Firebase auth token
   - User identity verified before processing payment

---

## Testing

### Non-Production Prices
In development/staging (non-production environment):
- Pro Monthly: $0.20 (1% of $20)
- Lifetime Plus: $5.00 (1% of $500)

This allows testing without spending large amounts of ZEC.

### Testnet Usage
The Rust backend can be configured to connect to ZCash testnet via:
- Environment variable: `ZCASH_NETWORK=test`
- lightwalletd testnet endpoint

---

## Configuration

### Environment Variables

**Proxy (Go):**
- `ZCASH_BACKEND_URL` - ZCash backend address (default: `http://54.210.176.154:8080`)
- `KRAKEN_API_URL` - Kraken ticker endpoint (fixed: `https://api.kraken.com/0/public/Ticker?pair=ZECUSD`)

**Backend (Rust):**
- `ZCASH_NETWORK` - `"mainnet"` or `"testnet"` (default: mainnet)
- `HTTP_BIND` - Address to listen on (default: `0.0.0.0:8080`)
- `UFVK` - Unified Full Viewing Key (required)
- `WALLET_DB_PATH` - Path to wallet.db (default: `./wallet.db`)
- `INVOICE_DB_PATH` - Path to invoice.db (default: `./invoices.db`)
- `CACHE_DB_PATH` - Path to block cache (default: `./cache.db`)
- `LWD_URL` - lightwalletd gRPC endpoint
- `POLL_SECONDS` - Sync interval (default: 15 seconds)
- `SYNC_BATCH_SIZE` - Blocks per sync (default: 100)

---

## File Locations

### iOS
- `StoreLib/Sources/StoreLib/Services/API/ZCashAPIClient.swift` - API client
- `Chats/Sources/Chats/Presentation/Pages/ZCashPayment/ZCashPaymentViewModel.swift` - ViewModel
- `Chats/Sources/Chats/Presentation/Pages/ZCashPayment/ZCashPaymentScreen.swift` - UI

### Proxy (Go)
- `enchanted-proxy/internal/zcash/handler.go` - HTTP handlers
- `enchanted-proxy/internal/zcash/service.go` - Business logic + Kraken integration

### Backend (Rust)
- `zcash-payment-backend/src/main.rs` - Complete backend implementation

---

*Last updated: December 24, 2024*
