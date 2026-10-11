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
