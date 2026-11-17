# book-maker: quoting EURC-30NOV26 on the OnchainBook

`cmd/book-maker` keeps one spline on risk-core's `OnchainBook` (exchange repo,
`contracts/risk-core/src/periphery/OnchainBook.sol`; spec `docs/eurc-usdc-futures-spec.md`, Part B). It is a
separate process from `mm-bot`, which keeps quoting the same series through `Matching`.

## What it does

- **Startup (preflight).** It refuses to start unless:
  - the account `MM_BOOK_ACCOUNT_ID` is owned by the signer (not by `Matching`) and is under the market's engine;
  - the book allows the signer as a maker (the vault's setup batch);
  - the account grants the book at least 2^128 on the future (both directions) and on cash (paying).

  It then sets the curve if the stored one differs from `MM_BOOK_CURVE`, and registers the account if it is not
  registered. Both are cheap and repeatable. It never moves funds or grants allowances.
- **Each cycle (`MM_BOOK_POLL_SECONDS`).** It reads three prices: the index (the future's `EurcIndexFeed`), the
  series' forward (the forward feed the keeper publishes) and the reference (the engine's Chainlink push feed for
  market `MM_BOOK_REFERENCE_MARKET_ID`). The spline's mid is the forward, sent as `round((forward − index) / tick)`
  ticks from the index, which the book re-reads at every take.
- **When it sends an update.** It sends `updateSplines` only when one of these holds:
  - the offset moved `MM_BOOK_REPRICE_TICKS`;
  - its configuration changed;
  - the spline is down;
  - the spline is `MM_BOOK_REFRESH_SECONDS` old.
- **When it takes the spline down.** It cancels the spline when any of these holds:
  - the series is within `MM_BOOK_CUTOFF_SECONDS` of last trade;
  - any of the three feeds fails to answer (they revert when stale);
  - the index and the reference disagree by more than `MM_BOOK_MAX_DIVERGENCE_BPS`. This is the DEX-skew guard,
    spec B14a.

  A spline that is already down costs nothing. The reason is logged at info whenever it changes.
- **Hand-off between processes.** `seq` is the wall clock in milliseconds (and always above the stored one). A new
  process claims the spline with one update on its first cycle. An older process that sees a seq it did not write
  stops (`superseded`, exit 0) and does not cancel. At SIGTERM a process cancels the spline only if the stored seq
  is still its own. A draining container therefore never quotes over, or takes down, its successor.
- **Shutdown.** It cancels its spline. If the cancel fails, the spline lapses at its ttl (`MM_BOOK_TTL_SECONDS`).

## One-time setup (runbook; moves funds, so by hand)

The owner of the maker account signs and pays gas for every update. An update is about 40k gas; at the default
refresh of 30 minutes plus repricing, that is a small fraction of the EURC v3 relayer's spend.

1. The vault's setup batch allows the owner (`propose_onchain_book_batch.py book-setup`, exchange repo).
2. `SubAccounts.createAccount(owner, engineV2)`. The account must not be deposited into `Matching`.
3. Approve USDC to the engine's cash, then `CashAsset.deposit(account, amount)`.
4. `SubAccounts.setAssetAllowances(account, book, [(future, max, max), (cash, max, max)])`. Revoking is the same
   call with 0. Transferring the account NFT drops the allowances.
5. Fund the owner with ETH for gas, and add it to `GAS_WATCH` and `check_signer_balance.py` the same day.

## Where it runs

On ECS as `market-maker-book` (exchange `infra/aws/market-maker-book.tf`), from the same image as the other makers
(`/app/book-maker`, the entrypoint overridden), with the same `mm_private_key` and `mm_rpc_url` secrets. Not on the ops
box: the box may read the maker key only for the length of one apply (`ops_box_mm_key_grant`). Its stop timeout leaves
SIGTERM time to cancel the spline.

## Configuration

| Env | Default | |
|---|---|---|
| `MM_RPC_URL` | required | |
| `MM_BOOK_ADDRESS` | required | from `ONCHAIN_BOOK.json` |
| `MM_BOOK_ACCOUNT_ID` | required | the account above |
| `MM_BOOK_CURVE` | required | `lots@offsetTicks,...`, e.g. `200@0,400@50`; at most 10 regions, offsets not decreasing |
| `MM_BOOK_HALF_SPREAD_TICKS` | required | ticks each side of the mid |
| `MM_BOOK_DRY_RUN` | `true` | reads, decides and logs; sends nothing (preflight reports what it would set) |
| `MM_BOOK_MARKET_ID` | 1 | |
| `MM_BOOK_CURVE_ID` | 1 | both sides |
| `MM_BOOK_REFERENCE_MARKET_ID` | 2 | the engine market whose spot feed is the reference |
| `MM_BOOK_SKEW_TICKS_PER_LOT`, `MM_BOOK_MAX_SKEW_TICKS` | 0 | on-chain inventory skew (B7) |
| `MM_BOOK_REFILL_LOTS_PER_SECOND` | 0 | 0: a consumed level returns at the next update |
| `MM_BOOK_TTL_SECONDS` | 3600 | at most the market's `maxTtl` |
| `MM_BOOK_REFRESH_SECONDS` | 1800 | under the ttl |
| `MM_BOOK_REPRICE_TICKS` | 20 | |
| `MM_BOOK_CUTOFF_SECONDS` | 3600 | before the series' last trade |
| `MM_BOOK_MAX_DIVERGENCE_BPS` | 50 | |
| `MM_BOOK_POLL_SECONDS` | 5 | |
| `MM_SIGNER_BACKEND` | `local` | `kms` with `MM_KMS_KEY_ID`, or `local` with `MM_OWNER_PRIVATE_KEY` |
| `MM_BOOK_HEARTBEAT_URL` | unset | a dead-man check (healthchecks.io), pinged at most once a minute while the book is quoted: an update landed, a live spline needed nothing, or the deliberate stop before last trade. An error, dry run, or a spline held down by an unreadable or diverging feed sends no ping, so the check alerts when that lasts. A secret; never logged |

## Rehearsed

On 2026-11-16, on an anvil fork of Base (block ~58.25M), against the book deployed by `deploy-onchain-book.s.sol`
with both vault batches landed:

- preflight set the curve and registered account #56;
- the first cycle posted a spline at +495 ticks (the live forward's basis) with a 100-tick half-spread;
- a 150-lot take from account #57 filled against it (taker +0.15 contracts, maker −0.15);
- SIGTERM cancelled the spline;
- with `MM_BOOK_MAX_DIVERGENCE_BPS=1` (index and reference were ~3.5 bps apart) it never quoted.
