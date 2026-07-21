# Market-maker → mainnet SEP-16 reconfig runbook

Repoint the existing `market-maker` Railway service from its broken config (Base **Sepolia** +
the abandoned `0xC7bE60` key + spot-only, every order 400s) to **quote the live SEP-16
deliverable FX future on Base mainnet**. The MM already supports FX futures (APR30-2026 is a
built-in intended symbol) and resolves market specs dynamically from `/v1/markets`, so this is a
config + funded-subaccount job, not a code change — with one caveat on pricing (§4).

Live addresses (Base 8453): matching `0x9E90A9cD13d859Bd6a08168082FB1F6F7405F191`, trade
`0x44813aD30b2fFC1bB2871Eed9b19F63c8196eD1c`, subAccounts `0x7019244E25FA416e6Ca2ed2F3cA25277aef72843`,
manager `0xcE01f3D74400caE39bd7608cd2d286C2e3874d49`, cash `0x6B232A2155Bd0C9bf741dB4cf8E7e8A0176A6fc6`,
USDC `0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913`, future `0xDd9c2Ddf97a2Dc9B9d348DcD0ef776aF5291A1F9`
(subId `1789567201`). SEP-16 symbol: `USDCcNGN-SEP16-2026`.

## 1. New MM signer key (rotate — do NOT reuse 0xC7bE60)
The old `MM_OWNER/SIGNER_PRIVATE_KEY` (for `0xC7bE60…`) is abandoned **and** was exposed in the
Railway env — treat it as compromised. Generate a fresh EOA for the MM:
```bash
cast wallet new     # note the address + private key; keep the key OUT of chat/git
```
Fund the new address with a little Base ETH for the create/deposit txs (~0.002 ETH).

## 2. Create + fund the MM's subaccount (on-chain, once)
From `services/execution` in the monorepo, using the NEW MM key and live-address overrides:
```bash
export RPC_URL='<Base mainnet RPC>'
export PRIVATE_KEY='0x<new MM key>'
export CHAIN_ID=8453
export MANAGER_ADDRESS=0xcE01f3D74400caE39bd7608cd2d286C2e3874d49
export MATCHING_ADDRESS=0x9E90A9cD13d859Bd6a08168082FB1F6F7405F191
# a) create a subaccount owned by the MM key
bash scripts/create_subaccount.sh          # -> prints the new ACCOUNT_ID
# b) deposit USDC cash margin (the future is cash-margined until delivery)
export ACCOUNT_ID=<new account id>
export CASH_ADDRESS=0x6B232A2155Bd0C9bf741dB4cf8E7e8A0176A6fc6   # live cash (script default is stale)
export USDC_ADDRESS=0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913
export AMOUNT_USDC=<e.g. 200>              # DECISION: quoting capital
bash scripts/deposit_cash.sh
```
Then confirm it's trade-ready (owned by matching, has cash) exactly like accounts 7/8:
```bash
cast call 0x7019244E25FA416e6Ca2ed2F3cA25277aef72843 "ownerOf(uint256)(address)" $ACCOUNT_ID --rpc-url $RPC_URL
cast call 0x7019244E25FA416e6Ca2ed2F3cA25277aef72843 "getBalance(uint256,address,uint256)(int256)" $ACCOUNT_ID 0x6B232A2155Bd0C9bf741dB4cf8E7e8A0176A6fc6 0 --rpc-url $RPC_URL
```
> Verify create_subaccount.sh leaves the account in the **matching-deposited** state (owner =
> `0x9E90A9cD…`), matching how 7/8 look. If it doesn't, the account also needs the deposit-into-
> matching step before the matcher can trade it — mirror the 7/8 setup.

## 3. Repoint the Railway `market-maker` env
Set on the `market-maker` service (secrets as SecureString), then redeploy:
```
MM_CHAIN_ID=8453
MM_RPC_URL=<Base mainnet RPC>              # NOT sepolia
MM_MARKET_SYMBOL=USDCcNGN-SEP16-2026
MM_MATCHING_ADDRESS=0x9E90A9cD13d859Bd6a08168082FB1F6F7405F191
MM_TRADE_MODULE_ADDRESS=0x44813aD30b2fFC1bB2871Eed9b19F63c8196eD1c
MM_SUBACCOUNTS_ADDRESS=0x7019244E25FA416e6Ca2ed2F3cA25277aef72843
MM_SUBACCOUNT_ID=<new account id>
MM_OWNER_ADDRESS / MM_SIGNER_ADDRESS=<new MM EOA>
MM_OWNER_PRIVATE_KEY / MM_SIGNER_PRIVATE_KEY=<new key>
MM_API_BASE_URL=http://markets-service.railway.internal:8080   # internal DNS, not 127.0.0.1
MM_DATABASE_URL=<same Postgres as markets-service>             # it reconciles active_orders directly
```
Remove any `MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_*` (spot-only) vars.

## 4. Reference price — the one caveat, and the bootstrap that sidesteps it
The MM quotes around a reference = local book mid (preferred) or an external anchor. The spot
market bootstraps from an er-api anchor, but that path is **spot-gated** — a fresh SEP-16 book has
no mid, so the MM has nothing to anchor to and won't post the first quotes.

**Bootstrap: seed one non-crossing pair manually** (via the now-fixed `generate_trade_order.mjs`)
so the book has a mid; the MM then adopts it and maintains from there:
```bash
# in services/execution, env from the manual-order runbook, using accounts 7/8:
#   bid a tick BELOW mark:  SIDE=buy  LIMIT_PRICE=1378000000000000000000  (acct 7)
#   ask a tick ABOVE mark:  SIDE=sell LIMIT_PRICE=1380000000000000000000  (acct 8)
# (non-crossing: 1378 bid < 1380 ask, so they rest and don't fill)
```
Longer-term, wire a proper future anchor (mark-price feed) so it self-bootstraps — likely a small
MM code change (extend the anchor to read the on-chain mark for futures). Track separately.

## 5. Verify (after redeploy)
```bash
railway logs --service market-maker    # expect "place order" for USDCcNGN-SEP16-2026, NO 400s
```
On-chain / matcher: the SEP-16 book should now show `has_bid=true has_ask=true`, and MM quotes
rest. A real counterparty order that crosses settles through the path proven 2026-07-21.

## Decisions to make before running
- **Quoting capital** (`AMOUNT_USDC`) for the MM subaccount.
- **Reference price**: bootstrap-by-seeding now (§4) vs. a mark-anchor code change.
- Spot MM: a **second** Railway instance from `numofx/market-maker` later (one market per process).
