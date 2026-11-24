# Apple purchases

An App Store purchase grants its plan to one account at a time, its holder. Every
account a purchase has been attached to shares one quota group, and every usage limit
counts over the group. Code: `internal/iap` (attach, policy, review) and
`internal/quota` (groups).

## Attach

`POST /api/v1/subscription/appstore/attach` with `{"jwsTransactionInfo": "<JWS>"}`. The
iOS app sends each of its purchases at every launch and after a purchase or restore.

1. **Signature.** The JWS must verify up to Apple's root certificate, and its leaf and
   intermediate certificates must carry Apple's App Store signing markers (OIDs
   `1.2.840.113635.100.6.11.1` and `1.2.840.113635.100.6.2.1`).
2. **Policy.** The transaction must be for `APPSTORE_BUNDLE_ID`, made in production (or in
   the sandbox by an account allowed below), for a known product, not family-shared, not
   replaced by an upgrade, not revoked and, for a subscription, not expired.
3. **Confirmation.** A purchase seen for the first time, or reaching an account other
   than its holder, is read from the App Store Server API first. If the App Store cannot
   be reached, nothing is written and the app gets a 503; it tries again at its next
   launch.
4. **Binding**, in one database transaction with the purchase's row locked:
   - a first attach records the purchase with the caller as holder;
   - when another account holds it, that account goes back to Free (only if its plan
     came from this purchase), the caller becomes the holder, and the two accounts'
     quota groups are joined;
   - each first attach and move is recorded in `apple_purchase_events`.
5. **Blocking.** A purchase that reaches more than `APPSTORE_PURCHASE_MAX_ACCOUNTS`
   accounts in 30 days is blocked (`too_many_accounts`). So is a purchase Apple reports
   refunded or revoked (`revoked`). A blocked purchase grants nothing: every account
   whose plan came from it is set to Free. Reaching more than 3 accounts is logged.
6. **Plan.** The caller's entitlement is written from the purchase unless the caller holds
   a better plan, or as good a plan from another provider or another purchase. Plus and
   Pro sent in either order give Pro.

| Answer | When |
|---|---|
| 200, `status: true`, `expiresAt` | Accepted (also when a better plan the caller holds was kept) |
| 200, `status: false` | The purchase is blocked |
| 403, `reason: purchase_refused` | The policy refused it; `error` is a sentence the app can show |
| 400 | Not a valid signed transaction |
| 503 | The App Store could not confirm it |

## Quota groups

`quota_group_members` holds one row per account in a group; an account with no row is a
group of one. Chat plan tokens, deep research runs and task limits all count over the
caller's group; the plan is the caller's own. The task reconciler walks a group's tasks
against the best plan any member holds (`docs/task-reconciler.md`). Each server instance
caches a group for a minute; a join applies at once on the instance that made it.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `APPSTORE_BUNDLE_ID` | (none) | The app whose purchases are accepted. Without it every purchase is refused. |
| `APPSTORE_SANDBOX_EMAIL_DOMAINS` | `eternis.ai` | Accounts whose verified email is on one of these domains may use sandbox purchases. |
| `APPSTORE_SANDBOX_EMAILS` | (none) | Further accounts, by verified email, that may use sandbox purchases. |
| `APPSTORE_TEST_PRODUCTS` | `false` | Accept the staging and development products (`.staging`, `.development`). |
| `APPSTORE_PURCHASE_MAX_ACCOUNTS` | `5` | Accounts one purchase may reach in 30 days; the next one blocks it. At least 2. |
| `ENTITLEMENT_REVIEW_MODE` | `off` | `off`, `dry-run` or `enforce`; see below. |
| `ENTITLEMENT_REVIEW_CUTOFF` | `2026-02-15` | Accounts created on or before this day (UTC) keep an unrecorded plan. |

All are on the enclave allowlist (`deploy/enclaver.yaml`).

## Plans from before purchases were recorded

Apple entitlements written before this change have no purchase on record
(`entitlements.apple_original_transaction_id` is NULL). They keep working for chat. Deep
research refuses them with a request to open the iOS app once, which sends the purchase
and records it.

The **entitlement review** handles the Apple Plus ones once. It runs two minutes after the
server starts, on one instance, and looks each owner up in Firebase Auth:

- an account that no longer exists, or was created after the cutoff, is set to Free;
- the others keep the plan.

`dry-run` only reports. `enforce` first copies the entitlements table to
`entitlements_review_backup`, then resets; it runs once (later starts skip it) unless it
failed. It resets nothing when most owners are missing or could not be looked up, which
points at the wrong Firebase project. Each run's report is in `entitlement_review_runs`:

```sql
SELECT id, started_at, mode, report->'candidates', report->'kept',
       jsonb_array_length(report->'deleted'), jsonb_array_length(report->'created_after_cutoff'),
       report->'reset', report->'errors'
FROM entitlement_review_runs ORDER BY id DESC LIMIT 5;
```

Someone reset who holds a real purchase gets the plan back when the app next sends it.
To restore one account from the backup:

```sql
UPDATE entitlements e
SET subscription_tier = b.subscription_tier, subscription_expires_at = b.subscription_expires_at, updated_at = NOW()
FROM entitlements_review_backup b
WHERE b.run_id = <run id> AND b.user_id = e.user_id AND e.user_id = '<user id>';
```

## Operations

A purchase and its history:

```sql
SELECT * FROM apple_purchases WHERE original_transaction_id = '<id>';
SELECT kind, user_id, previous_user_id, reason, created_at
FROM apple_purchase_events WHERE original_transaction_id = '<id>' ORDER BY id;
```

Unblocking by hand: clear the block and record why. The accounts counted toward the limit
start again from the unblock.

```sql
BEGIN;
UPDATE apple_purchases SET blocked_at = NULL, block_reason = NULL, updated_at = NOW()
WHERE original_transaction_id = '<id>';
INSERT INTO apple_purchase_events (original_transaction_id, kind, user_id, reason)
VALUES ('<id>', 'unblock', '<holder user id>', '<why>');
COMMIT;
```

The holder gets the plan back at the app's next launch.

Log lines (component `iap`): `apple purchase blocked` (with the reason and the accounts in
30 days), `apple purchase reached many accounts`, `purchase refused` (with the reason) and
`app store confirmation failed`.
