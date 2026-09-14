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
3. optionally fetches an external anchor price
4. computes reserved exposure from active orders
5. derives available balances
6. runs risk checks
7. computes one target bid and one target ask
8. cancels stale or wrong orders
9. places missing passive quotes using signed `POST /v1/orders` payloads

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
- `MM_OWNER_PRIVATE_KEY` (only with `MM_SIGNER_BACKEND=local`, the default)
- `MM_KMS_KEY_ID` (only with `MM_SIGNER_BACKEND=kms`)
- `MM_SUBACCOUNT_ID`

## Common Optional Environment Variables

- `MM_MARKET_SYMBOL`
- `MM_SIGNER_BACKEND` (`local` | `kms`, default `local`)
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
- `MM_ORDER_EXPIRY_SECONDS` (default `60`)
- `MM_EXPIRY_REPLACE_MARGIN_SECONDS` (default `15`; must be `> 0` and `< MM_ORDER_EXPIRY_SECONDS/2`)
- `MM_CONTROL_ADDR` (default `127.0.0.1:8081`)
- `MM_CONTROL_TOKEN` (control API is disabled when unset)
- `MM_STATE_FILE`
- `MM_POLL_INTERVAL_MS`
- `MM_QUOTE_REFRESH_INTERVAL_MS`
- `MM_ORDER_SIZE`
- `MM_HALF_SPREAD_BPS`
- `MM_INVENTORY_SKEW_BPS`
- `MM_MAX_LONG_INVENTORY`
- `MM_MAX_SHORT_INVENTORY`
- `MM_MAX_NOTIONAL_PER_SIDE`
- `MM_MAX_NET_INVENTORY`
- `MM_MAX_QUOTE_AGE_SECONDS`
- `MM_MAX_ANCHOR_DEVIATION_BPS`
- `MM_STALE_MARKET_DATA_TIMEOUT_SECONDS`
- `MM_STALE_BALANCE_TIMEOUT_SECONDS`
- `MM_STALE_ANCHOR_TIMEOUT_SECONDS`
- `MM_MIN_QUOTE_LIFETIME_SECONDS`
- `MM_MIN_PRICE_MOVE_BEFORE_REPLACE_BPS`
- `MM_MAX_CANCELS_PER_MINUTE`
- `MM_MIN_BASE_BALANCE`
- `MM_MIN_QUOTE_BALANCE`
- `MM_CANCEL_STALE_ORDER_THRESHOLD_BPS`
- `MM_ADOPT_SIZE_TOLERANCE`
- `MM_OPERATOR_MODE`
- `MM_ANCHOR_SOURCE_TYPE`
- `MM_ANCHOR_URL`
- `MM_ANCHOR_FIXED_PRICE`
- `MM_KILL_SWITCH_FILE`
- `MM_DRY_RUN`
- `MM_LOG_LEVEL`
- `MM_METRICS_ADDR`
- `MM_READINESS_MISSING_QUOTE_TIMEOUT_SECONDS`
- `MM_SOAK_LOG_INTERVAL_SECONDS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_PROVIDER`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BASE_URL`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_API_KEY`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_CHAIN_ID`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SELL_TOKEN`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BUY_TOKEN`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_AMOUNT`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_TIMEOUT_MS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_AGE_SECONDS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_DEVIATION_BPS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BOOTSTRAP_ONLY`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SPREAD_MULTIPLIER`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SIZE_MULTIPLIER`

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

With `MM_SIGNER_BACKEND=kms` the key lives in AWS KMS (`ECC_SECG_P256K1`, `SIGN_VERIFY`) and no private key is read from the environment. Credentials and region come from the standard AWS chain (the task role on Fargate); the role needs `kms:GetPublicKey` and `kms:Sign` on `MM_KMS_KEY_ID`. The bot derives the address at startup and exits if it differs from `MM_OWNER_ADDRESS` or `MM_SIGNER_ADDRESS` when those are set — cancels require signer == owner. KMS signatures are normalized to low-s and V is recovered locally. `cmd/futures-check` and `cmd/mm-bot-integration` still sign with local keys.

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

Startup reconciliation also respects operator mode:

- `bid-only` adopts or places only the bid side and cancels the ask side
- `ask-only` adopts or places only the ask side and cancels the bid side
- `pause` and `dry-run-health` do not keep live quotes working

The reconciliation result is deterministic and records:

- adopted bid order id
- adopted ask order id
- canceled order ids
- rejection reasons for non-adopted orders

The bot persists only local nonce progression in `MM_STATE_FILE`. That file is used to avoid owner+nonce reuse across restarts. It is not treated as authoritative for live orders; Postgres and onchain balances are.

Persisted fields now include:

- nonce progression by side
- last submitted bid order id
- last submitted ask order id
- last adopted bid order id
- last adopted ask order id
- last halt reason
- last inventory snapshot
- control-API kill and pause

## Balance And Exposure Accounting

The bot reads total balances from `SubAccounts.getAccountBalances(accountId)` and computes:

- `total`: onchain subaccount balance
- `reserved`: open-order exposure inferred from active orders
- `available`: `total - reserved`

Sizing rules:

- bids are capped by available quote balance divided by bid price
- asks are capped by available base balance
- inventory caps still apply after available-balance capping

## Anchor Price And Fair Value

The bot can consume an external anchor price:

- `MM_ANCHOR_SOURCE_TYPE=none`
  - disables external anchor pricing
- `MM_ANCHOR_SOURCE_TYPE=fixed`
  - uses `MM_ANCHOR_FIXED_PRICE`
- `MM_ANCHOR_SOURCE_TYPE=http`
  - fetches `MM_ANCHOR_URL?market=<symbol>`
  - accepts either `{"price":123.45}` or a plain numeric response body

Reference price selection is:

1. anchor price when available
2. local mid from top of book
3. last trade
4. otherwise no quote

The bot also computes a local reference from the market itself. If local price deviates from anchor by more than `MM_MAX_ANCHOR_DEVIATION_BPS`, quoting halts and managed orders are canceled. `USDCcNGN-SPOT` is exempt: its book is the price, and its external anchor is a bootstrap only (below).

Anchor freshness is tracked independently from exchange market-data freshness. `MM_STALE_ANCHOR_TIMEOUT_SECONDS` controls how long the bot will tolerate an old anchor before halting with `anchor data stale`.

### USDC/cNGN Spot External Bootstrap Anchor

`USDCcNGN-SPOT` has a narrow bootstrap-only external anchor path to seed an otherwise empty local spot book.

It is enabled only with:

- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED=true`
- `MM_MARKET_SYMBOL=USDCcNGN-SPOT`

The bot then uses the configured external anchor as an indicative mark when and only when the local spot market has no usable local reference.

External-anchor selection for `USDCcNGN-SPOT` is:

1. mid of other participants' best bid and ask (the bot's own resting quotes are excluded)
2. external fallback price (`cngn-rate-picker` in production; see below)
3. local last trade, however old
4. otherwise halt with `reference price unavailable`

The book is the source of truth for spot, traders' resting orders included — but not the bot's own
quotes, which would otherwise pull a one-sided ladder toward the single order opposite it. The external anchor is the
fallback when other participants quote no two-sided book, ahead of the venue's last trade; it is never compared against the book, so
`MM_MAX_ANCHOR_DEVIATION_BPS` and `MM_STALE_ANCHOR_TIMEOUT_SECONDS` do not halt spot however far the
oracle is from the book. What bounds a mid set by a single trader's orders is the bot's inventory
and notional caps, not the oracle.

This path does not apply to:

- `USDCcNGN-APR30-2026`
- any other future market
- any other spot market

Bootstrap settings:

- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_PROVIDER`
  - `0x`
    - uses the 0x read-only price endpoint
  - `cngn-price-oracle`
    - reads the Base Chainlink cNGN/USD oracle described in `wrappedcbdc/cngn-price-oracle`
  - `cngn-rate-picker`
    - a Go port of [`wrappedcbdc/cngn-rate-picker`](https://github.com/wrappedcbdc/cngn-rate-picker) (v0.2.2):
      NGN per USDT from its keyless providers in priority order, first success wins (the library's
      threshold 1), each source failing over after 3 consecutive errors for 30s
    - Quidax `usdtngn`: 1h TWAP of 1-minute closes, counting only candles that traded, refused when the
      last trade is over 6h old
    - Textile `USDT_NGN`: 1h TWAP of cleared trades, else the book mid, else the last price
    - Bybit P2P (keyless web endpoint): mid of the modal buy/sell ad among reputable online advertisers
      within 2% of the median
    - sources are queried concurrently once a minute, each bounded by
      `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_TIMEOUT_MS`; no key or base URL is needed
    - departs from the library in reading Quidax `usdtngn` instead of `usdtcngn` and ignoring zero-volume
      candles: `usdtcngn` served a candle a minute with no volume, which the library's staleness guard accepts
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BASE_URL`
  - for `0x`: base price endpoint, for example a proxied 0x price endpoint
  - for `cngn-price-oracle`: not required by the on-chain oracle path
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_API_KEY`
  - optional direct 0x API key header for the read-only price endpoint
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_CHAIN_ID`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SELL_TOKEN`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BUY_TOKEN`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_AMOUNT`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_TIMEOUT_MS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_AGE_SECONDS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_DEVIATION_BPS`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BOOTSTRAP_ONLY`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SPREAD_MULTIPLIER`
- `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SIZE_MULTIPLIER`

Safety rules:

- non-`200` responses are rejected
- malformed payloads are rejected
- zero or negative prices are rejected
- cached anchor values older than `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_AGE_SECONDS` are rejected
- wild moves beyond `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_DEVIATION_BPS` from the last accepted external mark are rejected

For `cngn-price-oracle`, the bot reads the Base mainnet Chainlink cNGN/USD feed documented in the repo at:

- contract `0xdfbb5Cbc88E382de007bfe6CE99C388176ED80aD`
- reference endpoint shape if you run the repo's API server yourself: `GET /api/price`
- the bot converts the on-chain feed into `cNGN per USDC` before using it as the spot bootstrap mark

When the active reference source is the external bootstrap anchor:

- quote spread is widened by `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SPREAD_MULTIPLIER`
- quote size is reduced by `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SIZE_MULTIPLIER`

When `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BOOTSTRAP_ONLY=true`, the bot stops using the external anchor as soon as other participants quote a two-sided spot book. It keeps polling it, so the bootstrap price is warm if the book empties.

## Operator Modes

`MM_OPERATOR_MODE` supports:

- `normal`
  - standard two-sided quoting
- `bid-only`
  - quote only the bid side
- `ask-only`
  - quote only the ask side
- `pause`
  - cancel managed quotes and hold
- `dry-run-health`
  - refresh dependencies and compute health only; do not reconcile or quote

## Quote Churn Controls

The bot includes three controls to reduce churn in thin books:

- `MM_MIN_QUOTE_LIFETIME_SECONDS`
  - prevents replacing a quote until it has been live long enough
- `MM_MIN_PRICE_MOVE_BEFORE_REPLACE_BPS`
  - prevents replacing a quote for small drift even when the stale threshold is crossed
- `MM_MAX_CANCELS_PER_MINUTE`
  - halts quoting when replace-driven cancels exceed the configured per-minute budget

Cancels caused by risk halts, startup reconciliation, and explicit operator actions are still allowed. When a replace is suppressed by the lifetime or move guard, the bot logs that suppression and keeps the live order in place.

## Fill Accounting

Fill accounting uses this hierarchy:

1. open-order state truth
   - if a known live order disappears or its remaining size decreases, the bot records a fill on that order side
2. trade stream truth
   - if new market trades appear and no direct order-state delta is visible, the bot infers maker-side fills from aggressor side
3. inventory delta fallback
   - used only as a last-resort diagnostic signal

Partial fills are detected from real remaining-size changes in live open orders when that data is available.

## Quote Age Sources

The bot exposes both:

- local quote age
  - derived from the bot’s own `LastQuoteUpdate`
- exchange-observed quote age
  - derived from live order `created_at` timestamps when available

If exchange order timestamps are unavailable, exchange-observed quote age falls back cleanly to `0` and local quote age remains available.

## Kill Switch

If `MM_KILL_SWITCH_FILE` is set, the bot checks for that file on startup and on every cycle.

When the file exists:

- the bot cancels all managed orders in the selected market
- quoting halts immediately
- `/readyz` returns `503`
- metrics expose the halt reason `kill switch active`
- `MM_STATE_FILE` stores `last_halt_reason=kill switch active`

The file kill switch also reads `KILLED` on the control API, and `POST /control/resume` is refused while the file exists.

## Order Expiry

A cancel on this venue is off-chain only: TradeModule has no nonce invalidation, so a cancelled order stays executable on-chain until its signed `expiry`. The expiry, not the cancel, bounds exposure after a kill. Orders are therefore signed with a 60s expiry (`MM_ORDER_EXPIRY_SECONDS`), and a resting quote within `MM_EXPIRY_REPLACE_MARGIN_SECONDS` of its expiry is cancelled and re-placed in the same cycle (cancel category `expiry_replace`). That roll ignores `MM_MIN_QUOTE_LIFETIME_SECONDS`, does not count toward `MM_MAX_CANCELS_PER_MINUTE`, and bypasses the quote-refresh throttle.

## Control API

A JSON API for the operator terminal, served on `MM_CONTROL_ADDR` (default `127.0.0.1:8081`), separate from the metrics server. It listens only when `MM_CONTROL_TOKEN` is set; every request needs `Authorization: Bearer <token>` (401 otherwise). It binds to loopback because the terminal runs as a sidecar in the same Fargate task (awsvpc), so nothing outside the task needs to reach it.

| Endpoint | Body | Effect |
| --- | --- | --- |
| `GET /control/state` | | State, reason, sides, adjust, reference/book, target quotes, open orders, positions, config, last 50 place/cancel attempts |
| `POST /control/kill` | `{"reason"}` | `KILLED`; cancels every managed order immediately and returns `cancel_results` (`cancelled` / `not_found` / `error`). Idempotent. |
| `POST /control/pause` | `{"reason"}` | `PAUSED`; cancels managed orders |
| `POST /control/resume` | `{"reason","clear_kill"}` | Clears pause; clears a kill only with `clear_kill=true` (409 otherwise). Never overrides a risk halt. |
| `POST /control/side` | `{"side":"bid"\|"ask","enabled","reason"}` | Pulling a side cancels that side's orders and suppresses it |
| `POST /control/adjust` | `{"mid_shift_bps"?,"spread_add_bps"?,"size_mult"?,"reason"}` | Partial update. Bounds: `\|mid_shift_bps\| <= 200`, `spread_add_bps` in `[-(MM_HALF_SPREAD_BPS-1), 500]`, `size_mult` in `[0, 5]` |

State precedence is `KILLED > PAUSED > HALTED > RUNNING`. Every change wakes the quoting loop instead of waiting for the next poll. Prices are cNGN per USDC, sizes are USDC, and `buy` is a bid for USDC. `not_found` on a kill means the order was already off the book (filled, cancelled, or mid-settlement). In `MM_DRY_RUN` nothing is sent: cancel results read `error: "dry run: cancel not sent"` and `last_actions` entries carry `dry_run: true`.

**KILLED** survives an in-task restart (persisted in `MM_STATE_FILE` under `control`), but `/tmp` is ephemeral on Fargate, so a replaced task starts un-killed; the terminal re-asserts kill on connect. Even after a kill, orders already signed remain executable on-chain for up to `MM_ORDER_EXPIRY_SECONDS`. Side pulls and adjustments are not persisted.

Every control action is logged as `control_action` with the action, reason, resulting state and remote address.

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

### Futures Fair-Value HTTP Anchor

For futures markets (for example `USDCcNGN-APR30-2026`), run a local anchor service that derives fair value from spot:

`fair = spot * exp(r * T) + carry_abs + spot * (carry_bps / 10000)`

where:

- `spot` comes from `USDCcNGN-SPOT` top-of-book mid (or last trade fallback)
- `r` is `FAIR_ANCHOR_RATE_APR`
- `T` is years until `FAIR_ANCHOR_EXPIRY_UTC`

Start the anchor service:

```bash
export PORT=8080
export FAIR_ANCHOR_API_BASE_URL=https://<your-markets-service-public-url>
export FAIR_ANCHOR_FUTURES_SYMBOL=USDCcNGN-APR30-2026
export FAIR_ANCHOR_EXPIRY_UTC=2026-04-30T00:00:00Z
export FAIR_ANCHOR_RATE_APR=0.08
export FAIR_ANCHOR_CARRY_ABS=0
export FAIR_ANCHOR_CARRY_BPS=0

go run ./cmd/fair-anchor
```

Point the market maker to this HTTP anchor:

```bash
export MM_MARKET_SYMBOL=USDCcNGN-APR30-2026
export MM_OPERATOR_MODE=normal
export MM_DRY_RUN=false
export MM_ANCHOR_SOURCE_TYPE=http
export MM_ANCHOR_URL=http://<fair-anchor-service-internal-domain>/price?market=USDCcNGN-APR30-2026

go run ./cmd/mm-bot
```

Sanity-check the anchor output:

```bash
curl -s "http://127.0.0.1:8090/price?market=USDCcNGN-APR30-2026"
```

Health-check anchor usability (fails closed with `503` if spot/fair cannot be produced):

```bash
curl -s -i "http://127.0.0.1:8090/healthz"
```

Health and metrics:

- `GET /healthz`
- `GET /readyz`
- `GET /metrics`

`/healthz` is liveness-only and returns `200` while the process is running.

`/readyz` returns `503` when:

- the bot is halted
- anchor data is stale
- balances are stale
- exchange market data is stale
- a required quote side has been missing longer than `MM_READINESS_MISSING_QUOTE_TIMEOUT_SECONDS`

## Dry Run

With `MM_DRY_RUN=true`, the bot still loads books, trades, balances, and open orders, computes quotes, runs startup reconciliation logic, and logs all decisions. It does not submit live cancels or placements.

`MM_DRY_RUN` and `MM_OPERATOR_MODE=dry-run-health` are different:

- `MM_DRY_RUN=true` still runs the normal decision flow but suppresses live mutations
- `MM_OPERATOR_MODE=dry-run-health` skips live reconciliation and quoting entirely

## Metrics To Watch First

Start with:

- `mm_bot_halted`
- `mm_bot_halt_reason`
- `mm_bot_ready`
- `mm_bot_open_bid_present`
- `mm_bot_open_ask_present`
- `mm_bot_anchor_freshness_age_seconds`
- `mm_bot_exchange_market_data_freshness_age_seconds`
- `mm_bot_balance_freshness_age_seconds`
- `mm_bot_order_cancels_total_by_category`
- `mm_bot_fills_total`
- `mm_bot_partial_fills_total`
- `mm_bot_quote_age_seconds`
- `mm_bot_exchange_quote_age_seconds`
- `mm_bot_net_inventory`
- `mm_bot_live_quoted_spread_bps`
- `mm_bot_external_anchor_present`
- `mm_bot_external_anchor_age_seconds`
- `mm_bot_external_anchor_price`
- `mm_bot_external_anchor_refresh_total`
- `mm_bot_external_anchor_refresh_failures_total`
- `mm_bot_reference_source`

Healthy steady-state usually looks like:

- `mm_bot_halted == 0`
- `mm_bot_ready == 1`
- freshness-age gauges remain comfortably below their configured thresholds
- required quote sides are present for the active operator mode
- `risk_triggered` and `kill_switch` cancel categories stay at `0` during normal operation
- quote-age gauges move but do not grow without bound

## Overnight Soak Runs

Set `MM_SOAK_LOG_INTERVAL_SECONDS` to enable periodic status lines:

```bash
export MM_SOAK_LOG_INTERVAL_SECONDS=60
export MM_READINESS_MISSING_QUOTE_TIMEOUT_SECONDS=120
go run ./cmd/mm-bot
```

Each soak line includes quoting state, halt state, inventory, live bid/ask counts, fills since start, cancels since start, and dependency freshness ages. On shutdown the bot emits a final summary with uptime, fills, partial fills, cancels by category, halt count, last halt reason, and observed maxima for quote age, anchor deviation, and net inventory.

## Live Integration Harness

The repo includes a standalone live harness at `cmd/mm-bot-integration`.

It is intended for a real exchange stack where:

- `markets-service` API is reachable
- the matcher is running
- Postgres contains live `active_orders`
- RPC access to the deployed contracts is available
- you have one bot subaccount and one separate taker subaccount funded and ready
- the selected market can be kept isolated for the test

Additional required env vars for the harness:

- `MM_INT_TAKER_OWNER_PRIVATE_KEY`
- `MM_INT_TAKER_SUBACCOUNT_ID`

Additional optional env vars:

- `MM_INT_TAKER_SIGNER_PRIVATE_KEY`
- `MM_INT_TAKER_OWNER_ADDRESS`
- `MM_INT_TAKER_SIGNER_ADDRESS`
- `MM_INT_TAKER_RECIPIENT_ID`
- `MM_INT_SCENARIO`
- `MM_INT_TIMEOUT_SECONDS`
- `MM_INT_POLL_INTERVAL_MS`

Run it with:

```bash
export MM_API_BASE_URL=http://127.0.0.1:8080
export MM_RPC_URL=https://base-rpc.example
export MM_DATABASE_URL=postgres://...
export MM_CHAIN_ID=8453
export MM_OWNER_PRIVATE_KEY=0x...
export MM_SUBACCOUNT_ID=10
export MM_STATE_FILE=/tmp/mm-bot-integration-state.json
export MM_MARKET_SYMBOL=USDCcNGN-SPOT

export MM_INT_TAKER_OWNER_PRIVATE_KEY=0x...
export MM_INT_TAKER_SUBACCOUNT_ID=11
export MM_INT_SCENARIO=happy

go run ./cmd/mm-bot-integration
```

Supported `MM_INT_SCENARIO` values:

- `happy`
- `partial_fill`
- `restart_under_open_order`
- `stale_startup`
- `duplicate_startup`

Scenario behavior:

- `happy`
  - verifies one bid and one ask appear
  - crosses one side fully
  - verifies trade presence, inventory change, requote, and restart adoption
- `partial_fill`
  - verifies a partial fill on one resting side
  - verifies remaining live order state is sane
  - verifies inventory changes and no duplicate side orders appear
- `restart_under_open_order`
  - verifies managed quotes survive a clean stop
  - verifies restart adopts them
  - verifies the next sync cycle is stable and does not churn
- `stale_startup`
  - seeds intentionally stale bot-owned orders
  - verifies startup cancels them and fresh quotes are placed
- `duplicate_startup`
  - seeds duplicate bot-owned orders on one side
  - verifies startup cancels duplicates and converges to one order per side

Common preconditions:

1. the selected market must be isolated enough to be empty after the harness clears the bot and taker's own orders
2. both the bot and taker subaccounts must be funded sufficiently for the selected scenario
3. the matcher and persistence path must be live enough to update book, trades, and balances within the configured timeout

The harness fails loudly with actionable errors if required services are missing or the market is not isolated enough to run deterministically.

## Known Failure Modes

- If Postgres is unavailable, the bot cannot reconcile or compute reserved exposure correctly and will fail.
- If the deployment addresses or chain ID do not match the target environment, signed orders will be rejected.
- If the exchange changes `/v1/orders` payload validation or market metadata format, the client must be updated.
- If the anchor source fails and neither top-of-book nor last trade can provide a local fallback, the bot halts.
- For `USDCcNGN-SPOT`, if the local market has no two-sided book and has never traded, and the external bootstrap anchor is missing, stale, malformed, or rejected by the deviation guard, the bot halts with `reference price unavailable`.
- A spot order is a whole-cNGN engine amount, so a side whose inventory is worth under 1 cNGN at its price cannot quote. That side is suppressed and logged every quoting cycle as `quote side suppressed` (`insufficient_base_capacity` for asks, `insufficient_quote_capacity` for bids) — the low-inventory alert — and the other side keeps quoting.
- Available-balance accounting assumes open-order reserve semantics are:
  - bid reserves quote asset `size * price`
  - ask reserves base asset `size`
- Ownership is considered unambiguous only when the order matches the bot-managed order-id convention already used by this bot.
- Reconciliation adopts at most one order per side. Any duplicates on a side are canceled rather than partially adopted.
- The live integration harness requires an isolated market. If external orders remain in the book after cleanup, it aborts rather than running nondeterministically.
- The partial-fill scenario assumes the exchange leaves a remaining live resting order or a replacement state visible quickly enough to observe before timeout.

## Halt Reasons

The bot halts quoting and cancels managed orders in the selected market when any of these conditions occur:

- reference price unavailable
- exchange market data stale
- balances stale
- anchor data stale
- quote age exceeded
- anchor deviation exceeded
- open order notional exceeds limit
- inventory exceeds max long inventory
- inventory exceeds max short inventory
- available base balance below threshold
- available quote balance below threshold
- operator pause active
- kill switch active
- cancel rate limit exceeded

Dependency freshness is split in metrics and health diagnostics:

- `exchange market data stale`
  - local book, trades, or open-order refresh is too old
- `balances stale`
  - balance refresh is too old
- `anchor data stale`
  - external anchor refresh is too old

## Cancel Categories

Cancel metrics are split by category:

- `replace_driven`
  - quote maintenance cancel/replace activity
- `startup_reconciliation`
  - startup cleanup of stale, duplicate, malformed, or non-adopted orders
- `risk_triggered`
  - cancels triggered by risk halts, stale dependencies, or cancel-budget halt
- `kill_switch`
  - cancels triggered by the file-based kill switch or `POST /control/kill`
- `expiry_replace`
  - steady-state re-signing of quotes about to pass their signed expiry
- `control_side_pulled`
  - cancels from `POST /control/side` with `enabled=false`

`mm_bot_order_cancels_total` still exposes total cancels, while `mm_bot_order_cancels_total_by_category` shows the category breakdown.

## Verification

```bash
go test ./...
go build ./cmd/mm-bot
```

Live integration:

```bash
go run ./cmd/mm-bot-integration
```

What a passing run proves:

- `happy`: end-to-end quote placement, fill visibility, inventory movement, requote, and restart adoption
- `partial_fill`: partial fill handling without duplicate side orders
- `restart_under_open_order`: restart stability with live managed quotes
- `stale_startup`: stale managed orders are canceled, not adopted
- `duplicate_startup`: duplicate managed orders are cleaned up deterministically
