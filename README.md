# market-maker

Exchange-integrated Go market-making bot for Numo markets.

This bot keeps one market non-empty with one resting bid and one resting ask. It uses the same underlying exchange flow already present in the sibling repos:

- `markets-service` HTTP API for market metadata, order submission, book, trades, and owner+nonce cancels
- `markets-service` Postgres `active_orders` table for full live-order reconciliation
- `execution-contracts` / `risk-core` deployments plus RPC for EIP-712 order signing and subaccount balance reads

It supports one market per process. The intended symbols are:

- `USDCcNGN-SPOT`
- `USDCcNGN-APR30-2026`

## What The Bot Does

On startup it:

1. loads the selected market metadata from `/v1/markets`
2. loads all active orders for the configured owner in the selected market from Postgres
3. loads book, trades, and balances to compute fresh target quotes
4. classifies every existing order deterministically
5. adopts safe existing quotes when they already match the current target closely enough
6. cancels only stale, duplicate, malformed, ambiguous, or strategy-incompatible orders
7. restores its local nonce state from `MM_STATE_FILE`

On each cycle it:

1. fetches book and recent trades from `markets-service`
2. fetches subaccount balances from chain
3. computes reserved exposure from active orders
4. derives available balances
5. runs risk checks
6. computes one target bid and one target ask
7. cancels stale or wrong orders
8. places missing passive quotes using signed `POST /v1/orders` payloads

## Repo Layout

```text
cmd/mm-bot/main.go
internal/config
internal/exchange
internal/marketdata
internal/strategy
internal/risk
internal/execution
internal/state
internal/metrics
```

## Required Environment Variables

- `MM_API_BASE_URL`
- `MM_RPC_URL`
- `MM_DATABASE_URL` or `DATABASE_URL`
- `MM_CHAIN_ID`
- `MM_OWNER_PRIVATE_KEY`
- `MM_SUBACCOUNT_ID`

## Common Optional Environment Variables

- `MM_MARKET_SYMBOL`
- `MM_SIGNER_PRIVATE_KEY`
- `MM_OWNER_ADDRESS`
- `MM_SIGNER_ADDRESS`
- `MM_RECIPIENT_ID`
- `MM_MATCHING_REPO_PATH`
- `MM_RISK_CORE_REPO_PATH`
- `MM_MATCHING_ADDRESS`
- `MM_TRADE_MODULE_ADDRESS`
- `MM_SUBACCOUNTS_ADDRESS`
- `MM_WORST_FEE`
- `MM_ORDER_EXPIRY_SECONDS`
- `MM_STATE_FILE`
- `MM_POLL_INTERVAL_MS`
- `MM_QUOTE_REFRESH_INTERVAL_MS`
- `MM_ORDER_SIZE`
- `MM_HALF_SPREAD_BPS`
- `MM_INVENTORY_SKEW_BPS`
- `MM_MAX_LONG_INVENTORY`
- `MM_MAX_SHORT_INVENTORY`
- `MM_MIN_BASE_BALANCE`
- `MM_MIN_QUOTE_BALANCE`
- `MM_CANCEL_STALE_ORDER_THRESHOLD_BPS`
- `MM_ADOPT_SIZE_TOLERANCE`
- `MM_DRY_RUN`
- `MM_LOG_LEVEL`
- `MM_METRICS_ADDR`

## Auth And Signing

Order placement follows the same signed action structure used by `execution-service/scripts/generate_trade_order.mjs`.

The bot:

- resolves `matching` and `trade` addresses from `execution-contracts/deployments/<chain>/matching.json` unless overridden
- resolves `subAccounts` from `risk-core/deployments/<chain>/core.json` unless overridden
- ABI-encodes `TradeModule.TradeData`
- signs the `Action` EIP-712 payload with domain:
  - `name=Matching`
  - `version=1.0`
  - `chainId=MM_CHAIN_ID`
  - `verifyingContract=matching`

If your deployment uses a dedicated signer/session key, set `MM_SIGNER_PRIVATE_KEY` and optionally `MM_SIGNER_ADDRESS`. Otherwise the owner key is used for both.

## Startup Reconciliation

The bot treats exchange state as the source of truth.

At boot it queries all active orders for the configured owner and selected market from Postgres, computes the fresh target quotes, and then reconciles.

An order is adopted only if all are true:

- it is clearly bot-owned by the bot-managed order id convention `mm:<market>:<side>:<nonce>`
- it is in the selected market
- its side is `buy` or `sell`
- it is the only adoptable order on that side
- its price is within `MM_CANCEL_STALE_ORDER_THRESHOLD_BPS` of the current target quote
- its size is within `MM_ADOPT_SIZE_TOLERANCE` of the target size
- adopting it does not violate current risk checks

Orders are canceled on startup if any are true:

- duplicate orders exist on one side
- ownership is ambiguous
- metadata is malformed
- price is too far from target
- size is too far from target
- the current strategy would not quote that side
- startup risk checks are halted

The reconciliation result is deterministic and records:

- adopted bid order id
- adopted ask order id
- canceled order ids
- rejection reasons for non-adopted orders

The bot persists only local nonce progression in `MM_STATE_FILE`. That file is used to avoid owner+nonce reuse across restarts. It is not treated as authoritative for live orders; Postgres and onchain balances are.

## Balance And Exposure Accounting

The bot reads total balances from `SubAccounts.getAccountBalances(accountId)` and computes:

- `total`: onchain subaccount balance
- `reserved`: open-order exposure inferred from active orders
- `available`: `total - reserved`

Sizing rules:

- bids are capped by available quote balance divided by bid price
- asks are capped by available base balance
- inventory caps still apply after available-balance capping

## Running

```bash
export MM_API_BASE_URL=http://127.0.0.1:8080
export MM_RPC_URL=https://base-rpc.example
export MM_DATABASE_URL=postgres://...
export MM_CHAIN_ID=8453
export MM_OWNER_PRIVATE_KEY=0x...
export MM_SUBACCOUNT_ID=10
export MM_MARKET_SYMBOL=USDCcNGN-SPOT

go run ./cmd/mm-bot
```

Health and metrics:

- `GET /healthz`
- `GET /metrics`

## Dry Run

With `MM_DRY_RUN=true`, the bot still loads books, trades, balances, and open orders, computes quotes, runs startup reconciliation logic, and logs all decisions. It does not submit live cancels or placements.

## Known Failure Modes

- If Postgres is unavailable, the bot cannot reconcile or compute reserved exposure correctly and will fail.
- If the deployment addresses or chain ID do not match the target environment, signed orders will be rejected.
- If the exchange changes `/v1/orders` payload validation or market metadata format, the client must be updated.
- Available-balance accounting assumes open-order reserve semantics are:
  - bid reserves quote asset `size * price`
  - ask reserves base asset `size`
- Ownership is considered unambiguous only when the order matches the bot-managed order-id convention already used by this bot.
- Reconciliation adopts at most one order per side. Any duplicates on a side are canceled rather than partially adopted.

## Verification

```bash
go test ./...
go build ./cmd/mm-bot
```
