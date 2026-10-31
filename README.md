# Web3 AMM DEX on Ethermint

> Learning/Experiment project: Automated Market Maker Decentralized Exchange built on Ethermint (Cosmos SDK + EVM)

## Architecture

```
┌─────────────────────────────────────────────┐
│                  Frontend                    │
│            (React / CLI / API calls)          │
└──────────────────┬──────────────────────────┘
                   │ REST API
┌──────────────────▼──────────────────────────┐
│            Go Backend Service                 │
│         (gin + go-ethereum JSON-RPC)          │
└──┬──────────┬──────────┬──────────┬──────────┘
   │          │          │          │
┌──▼──┐  ┌───▼──┐  ┌───▼───┐  ┌──▼────┐
│Token│  │AMMPool│  │Factory │  │Router │
│(ERC20)│ │(x*y=k)│  │       │  │(Swap) │
└─────┘  └──────┘  └───────┘  └───────┘
        │
┌───────▼───────┐
│   Ethermint   │
│  (Cosmos SDK  │
│   + EVM)      │
└───────────────┘
```

## Smart Contracts

| Contract | Description |
|----------|-------------|
| `Token.sol` | ERC20-compliant token |
| `AMMPool.sol` | Core AMM using x*y=k formula with 0.3% fee |
| `Factory.sol` | Creates new trading pools |
| `Router.sol` | Handles swaps and liquidity operations |

## Backend API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check |
| `/pools` | GET | List all pools |
| `/pools/:pool` | GET | Get pool info |
| `/swap` | POST | Execute swap |
| `/liquidity` | POST | Add liquidity |
| `/liquidity` | DELETE | Remove liquidity |
| `/events` | GET | Get recent events |

## Order Book Gateway (subproject 4)

The order-book gateway exposes the matching engine over HTTP/WebSocket.

### HTTP API

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST   | `/v1/orders` | signed | Submit an EIP-712 signed order |
| DELETE | `/v1/orders/:id` | signed | Cancel an order (signed) |
| GET    | `/v1/orders/:id` | none | Order status |
| GET    | `/v1/markets` | none | Market list |
| GET    | `/v1/book/:market?depth=N` | none | Depth snapshot |
| GET    | `/v1/accounts/:account/balances` | none | Balance / available / held |

### WebSocket

`/ws` — subscribe with `{"op":"subscribe","market":1,"account":"0x..."}`
(optional filters). Pushes `order`, `trade`, `depth`, and `degraded` frames.

### Signature format

Orders and cancels are EIP-712 typed data over domain
`Web3 Orderbook DEX` v1. Qty/MaxNotional are decimal strings; replay is
blocked by a per-account nonce and an expiry timestamp.

### Residual trust assumptions

Read endpoints are unauthenticated. nonce is a persistent append log; its
corruption cannot cause fund loss (the ledger's trade-id idempotency keys
still prevent double settlement). The sequencer is single-process.

## History Indexer (subproject 5)

The indexer persists trades, on-chain funding events, and state roots to bbolt,
and can rebuild an independent ledger from chain events to reconcile against the
running ledger.

### Query API (read-only, unauthenticated)

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/v1/trades?market=&account=&limit=` | Trade history |
| GET    | `/v1/fund-events?account=&asset=&limit=` | Deposit/withdrawal/exit history |
| GET    | `/v1/roots?from=&limit=` | State root history |
| POST   | `/v1/reconcile` | Rebuild from chain and reconcile per-asset totals |

### Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `INDEXER_DB_PATH` | (required for persistence) | bbolt file path |
| `INDEXER_POLL_INTERVAL` | 5 | chain-event poll interval (seconds) |

### Residual trust assumptions

Reconciliation compares **per-asset totals** only: matching legs are zero-sum and
absent from the chain, so per-account balances differ between a rebuilt ledger
and the running ledger. Per-account reconciliation requires per-trade Merkle
proofs on-chain (deferred).

## Quick Start

### 1. Install dependencies
```bash
npm install
go mod download
```

### 2. Start local blockchain node
```bash
npx hardhat node
```

### 3. Deploy contracts
```bash
npx hardhat run scripts/deploy.js --network localhost
```

### 4. Start Go backend
```bash
cd backend && go run main.go
```

### 5. Make API calls
```bash
curl http://localhost:8080/health
curl http://localhost:8080/pools
```

## Settlement + Ledger (orderbook DEX, subproject 1)

The `contracts/Settlement.sol` vault + `backend/ledger` package form the custody
and accounting foundation for the off-chain order book DEX:

- Users deposit ERC20 assets into the vault (`deposit`).
- The off-chain ledger tracks internal balances and exports a deterministic Merkle root.
- `StateAnchorService` commits that root on-chain (`commitStateRoot`).
- Users withdraw via Merkle proof (`withdraw`), or permissionlessly exit after a
  delay (`requestExit` / `finalizeExit`) even if the operator is offline.

Design: `docs/superpowers/specs/2026-10-11-orderbook-dex-settlement-ledger-design.md`

### Settlement environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `SETTLEMENT_ADDRESS` | — | deployed vault address |
| `OPERATOR_KEY` | — | operator signing key (hex) |
| `CHAIN_ID` | `13700` | chain id (matches `hardhat.config.js`) |
| `DATA_DIR` | `./data/settlement` | ledger WAL/snapshot directory |
| `REQUIRED_CONFIRMATIONS` | `1` | deposit confirmations before crediting |
| `ANCHOR_INTERVAL` | `30` | state-root anchor period (seconds) |
| `EXIT_DELAY` | `300` | forced-exit delay (seconds) |

### Running the tests

```bash
npx hardhat test          # contracts
cd backend; go test ./... # ledger + services + config
```

## Project Structure

```
D:\code\web3\
├── contracts\          # Solidity smart contracts
│   ├── Token.sol
│   ├── AMMPool.sol
│   ├── Factory.sol
│   └── Router.sol
├── backend\            # Go backend service
│   ├── go.mod
│   ├── main.go
│   ├── handlers\       # HTTP handlers
│   ├── services\       # Core business logic
│   └── models\         # Data structures
├── scripts\            # Deployment scripts
│   ├── deploy.js       # Hardhat deployment
│   └── deploy.go       # Go deployment utility
├── frontend\           # Frontend components
│   └── src\
├── hardhat.config.js   # Hardhat configuration
├── package.json        # Node.js dependencies
└── README.md
```

## Key Concepts

- **AMM Formula**: x * y = k (constant product market maker)
- **Fee**: 0.3% per swap (30/10000)
- **Liquidity**: LP tokens minted proportional to contribution
- **Slippage**: Minimum output amount enforced by user

## Learning Goals

1. Understand AMM mechanics (constant product formula)
2. Learn Solidity smart contract development
3. Explore Ethermint (Cosmos SDK + EVM compatibility)
4. Build Go backend services interacting with blockchain
5. Practice full-stack Web3 development

## Perpetual Futures (subproject 6a)

Perpetual markets trade positions (long/short) with isolated margin and
user-selected leverage. Margin is frozen in the market's quote asset on both
sides; PnL is realized into the ledger on close.

The kernel lives in its own `backend/perp` package; the spot `matching` package
is untouched (no file changed, no semantics shared):

```
backend/perp/
  market.go            # market definition, leverage bounds, validation
  order.go             # positions, orders, margin derivation
  book.go              # price-level + time-priority book
  engine.go            # matching loop, position updates, cancels, market discretion
  invariants_test.go   # kernel invariants and deterministic replay
backend/services/
  perp_order_book.go   # service wiring, instruction WAL, degrade, PnL settlement
  perp_journal.go      # perpetual instruction WAL
backend/config/config.go  # PERP_* configuration
```

### Known gaps (6a)
- No mark price or funding rate (6b — now implemented, see below); PnL uses the trade price.
- No liquidation engine (6c): a loss exceeding margin causes a fatal degrade
  rather than forced liquidation.

### Configuration
| Key | Default | Description |
|-----|---------|-------------|
| `PERP_MARKETS` | (empty) | `ID:SYMBOL:BASE:QUOTE:BASE_DECIMALS:MAX_LEVERAGE[:MIN_QTY]`, `;`-separated |
| `PERP_DATA_DIR` | (empty) | instruction WAL dir (empty = memory-only) |

Example: `PERP_MARKETS=1:ETH-PERP:2:1:18:10:1000000000000000` (market 1, base
asset 2, quote asset 1, 18 base decimals, 10x max leverage, minimum 0.001 base
units). A malformed value fails startup instead of silently trading a subset of
the configured markets.

## Mark Price + Funding (subproject 6b)

Perpetual PnL on close still uses the **fill price** (a closed trade settles at
the price it traded at). The **mark price** is the pricing reference for
unrealized PnL and for funding.

### Mark price

```
mid     = (bestBid + bestAsk) / 2         (one-sided uses that side; empty book -> index)
clamped = clamp(mid, index*(1-dev), index*(1+dev))
mark    = (index*wi + clamped*wb) / (wi + wb)
```

Defaults: `dev = 0.5%`, `wi = wb = 1`. Clamping the book contribution is what
stops a single large order on a thin book from dragging the mark.

### Funding rate

```
premium = (mark - index) / index
rate    = clamp(premium / intervalHours, -maxRate, +maxRate)
```

Positive rate: longs pay shorts. Each position pays
`floor(notional(size, mark) * |rate| / 1e18)`. A round is journaled **before**
any ledger mutation and every transfer's idempotency key carries the round
index, so replaying the WAL cannot pay a round twice.

A payer without enough **available** balance has the shortfall recorded as a
debt and retried in a later round. Funding never degrades the book the way a
failed PnL settlement does.

### Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `PERP_INDEX_PRICES` | (empty) | `ID:PRICE;...` static index prices. Empty = marks and funding off (pure 6a) |
| `PERP_TICK_INTERVAL` | `5` | price refresh interval (seconds) |
| `PERP_FUNDING_INTERVAL` | `3600` | funding period (seconds) |
| `PERP_MAX_PRICE_AGE` | `60` | index staleness threshold (seconds) |
| `PERP_MAX_MARK_STALENESS` | `300` | how long a stale index may reuse the last mark (seconds) |
| `PERP_MAX_FUNDING_RATE` | `7500000000000000` | funding-rate cap, 1e18-scaled (0.75%) |
| `PERP_MARK_MAX_DEVIATION` | `5000000000000000` | max book deviation from index, 1e18-scaled (0.5%) |

Example: `PERP_INDEX_PRICES=1:2000` enables marks and hourly funding for market 1
with an index price of 2000.

### Residual trust assumptions (6b)

1. **The oracle is an operator trust point.** v1 uses operator-configured index
   prices; a median-of-venues source or an on-chain oracle is future work.
2. **Unbalanced funding is not netted through a fund.** A round only moves what
   payers can cover; 6c introduces the insurance fund that absorbs the rest.
3. **Debts can accumulate.** An account may carry funding debt until it closes,
   at which point the debt is retried. 6c's liquidation engine addresses this.

## Liquidation + Insurance Fund (subproject 6c)

6a's worst failure mode was that one account's insolvency halted the whole
perpetual book (`WithdrawOp` fails → fatal degrade). 6c removes that.

### Maintenance margin

```
maintenanceMargin = ceil(notional(size, mark) * maintenanceRate / 1e18)
equity            = margin + unrealizedPnL(mark)
liquidatable      = equity < maintenanceMargin
```

Mark price is the trigger, never the last trade — a single print must not
liquidate anyone.

### Forced close

Every scan (default 1s) closes each liquidatable position with an opposite-side
market IOC order. The closing order freezes no new margin: the account has none
left to freeze. An unfillable position (no counterparty) stays open and is
retried on the next scan, reported as `partial`.

### Shortfall and the insurance fund

When a realized loss exceeds the account's balance:

1. the account is debited only what it holds (never negative),
2. the insurance fund's balance covers the difference,
3. a `perp.baddebt` event reports the amount.

The book stays live throughout. If the fund itself is exhausted the bad debt is
recorded and trading continues — an insolvent fund is an accounting fact, not a
reason to stop the market.

### Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `PERP_MAINTENANCE_MARGIN_RATE` | `5000000000000000` | maintenance margin rate, 1e18-scaled (0.5%) |
| `PERP_LIQUIDATOR_FEE` | `1000000000000000` | liquidation fee, 1e18-scaled (0.1%) |
| `PERP_INSURANCE_FUND_ACCOUNT` | built-in reserve account | account that absorbs shortfalls |
| `PERP_LIQUIDATION_INTERVAL` | `1` | liquidation scan interval (seconds) |

### Residual trust assumptions (6c)

1. **The insurance fund is operator-funded.** If it runs dry, bad debt
   accumulates; ADL is not implemented.
2. **Liquidation requires a fresh mark.** With a stale or absent index price the
   scan skips the market — liquidating on a stale price would punish the wrong
   people.
3. **Full close, not partial.** v1 closes the entire position at once.
4. **No fill guarantee.** Thin books can leave a position partially closed.
