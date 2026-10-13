# Backend

Go backend for the Web3 AMM DEX (gin HTTP API over go-ethereum JSON-RPC) plus
the off-chain settlement ledger and its services.

## Running

From `backend/`:

```powershell
go run .
```

The AMM HTTP API binds to `:PORT` (default `8080`) and dials `ETH_RPC_URL`
(default `http://localhost:8545`).

Stop with `Ctrl+C` (SIGINT) or SIGTERM. Before exiting, the process closes the
settlement stack: it stops the deposit indexer and state-anchor services and
releases the ledger WAL handle. A close error is logged, not treated as fatal.

## Settlement subsystem

The settlement stack is built, initialized, and started only when
`SETTLEMENT_ADDRESS` is set. When it is unset the process logs that settlement
is disabled and serves the AMM API exactly as before — this is **not** an error.

| Variable | Default | Meaning |
|----------|---------|---------|
| `SETTLEMENT_ADDRESS` | — | deployed `Settlement` vault address; unset disables settlement |
| `OPERATOR_KEY` | — | operator signing key (hex); required when settlement is enabled. **Never logged** |
| `CHAIN_ID` | `13700` | chain id used to build and sign transactions |
| `DATA_DIR` | `./data/settlement` | ledger snapshot/WAL directory; defaults under `backend/data/`, which is gitignored |
| `REQUIRED_CONFIRMATIONS` | `1` | deposit confirmations required before crediting |
| `ANCHOR_INTERVAL` | `30` | state-root anchor period, in seconds (also the indexer poll period) |
| `EXIT_DELAY` | `300` | forced-exit delay, in seconds (loaded into `SettlementConfig`) |

`RPC_URL` (default `http://localhost:8545`) is supplied by the app config
because `SettlementConfig` carries no RPC endpoint. `OPERATOR_KEY` is never
written to logs.

## Fast-withdraw retry semantics

`SettlementService.Withdraw` debits the off-chain ledger **before** it submits
the on-chain transaction, and the debit is idempotent by the caller-supplied
`ref`. As a result:

- A failed submit leaves the balance debited; do not roll it back.
- Retry by reusing the **same `ref`**: the retry does not debit a second time,
  but it does re-send the transaction.
- The payout completes only after a **new anchored root containing the
  post-debit balance** exists. The proof is taken before each debit, so the
  withdrawal must reference the anchored `rootIndex` for that post-debit root.

## Order book and freeze semantics

The off-chain order book (`OrderBookService`) sits on top of the ledger and the
pure matching kernel. Orders freeze funds at placement so a fill is always
settleable:

- **Available = ledger balance − frozen.** A placed limit buy freezes
  `ceil(qty × price / 10^baseDecimals)` quote, a limit/market sell freezes `qty`
  base, and a market buy freezes its `MaxNotional`.
- **Freeze first, ledger before release.** `Place` runs: compute reserve →
  `Holds.Reserve` (a rejection leaves the book untouched) → append the
  instruction to the WAL → match → settle each fill as **one atomic ledger
  `Apply`** (base seller→buyer, quote buyer→seller) → release residual freezes →
  publish events. The ledger transfer always precedes the freeze release, so the
  available balance is never briefly inflated (which would let a withdrawal race
  a transfer into a half-settled trade).
- **Withdrawals only take available funds.** `SettlementService.Withdraw` routes
  its balance check and `WithdrawOp` through `Holds.Spend`, which holds the hold
  lock across both. A withdrawal therefore cannot touch a resting order's freeze
  and cannot race a new order freeze into overspending.
- **Zero fees (v1).** A fill is exactly two zero-sum legs, so `Σ balance` per
  asset is unchanged by trading.
- **Crash recovery.** The instruction WAL is the single source of truth: a
  restart replays it into a fresh kernel, catches up any fill the crash left
  unsettled (idempotent by `trade:<id>:base` / `trade:<id>:quote`), and sets the
  freeze state once from the kernel's derived open reserves. A ledger `Apply`
  failure during live operation puts the service into fatal degradation: it
  refuses further instructions (`ErrServiceDegraded`) until a restart replays.

## Order book environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `ORDERBOOK_DATA_DIR` | — | order-book instruction WAL (`orders.wal`) directory. Empty means memory-only: the book runs but cannot be rebuilt after a restart |
| `ORDERBOOK_MARKETS` | — | market definitions; unset disables the order book entirely (no `OrderBookService`) |

`ORDERBOOK_MARKETS` is `ID:SYMBOL:BASE_ASSET:QUOTE_ASSET:BASE_DECIMALS[:MIN_QTY]`,
with several markets separated by `;`. Example:
`1:ETH/USDT:2:1:18:1000000000000000` (base asset 2, quote asset 1, 18 base
decimals, minimum 0.001 ETH). A malformed string is a hard startup failure
(`BuildSettlementStack` returns an error), never a silently skipped market.

## Residual trust assumptions

1. **Forced exit vs resting orders.** A forced exit pays out the account's
   *total* balance (the published Merkle root includes frozen funds). The
   operator MUST cancel a user's resting orders after observing that user's
   `requestExit`; otherwise an order backed by funds that have left the vault can
   fill and the ledger records a balance the vault cannot cover. v1 relies on the
   operator cancelling on the exit event (order TTL / auto-cancel on exit is
   deferred).
2. **The operator can cancel and can stop.** Matching is off-chain, so the
   operator may deny service, cancel orders, or halt matching. This matches
   subproject 1's model: the operator cannot cause a user fund *loss*, but can
   refuse service.
3. **The instruction log is a single point of failure.** If the order-book WAL is
   lost or unrecoverably corrupted, the freeze state cannot be rebuilt (the
   ledger itself is unaffected and totals remain withdrawable). Back it up;
   swapping in bbolt/Postgres later does not change the kernel.
