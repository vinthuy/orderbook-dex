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
