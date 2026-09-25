# API Reference

## Base URL
`http://localhost:8080`

## Endpoints

### Health Check
```
GET /health
Response: { "status": "ok" }
```

### Get Pools
```
GET /pools
Response: [
  {
    "address": "0x...",
    "token0": "0x...",
    "token1": "0x...",
    "reserve0": "1000000",
    "reserve1": "2000000"
  }
]
```

### Swap
```
POST /swap
Body: {
  "pool": "0x...",
  "tokenIn": "0x...",
  "amountIn": "1000000",
  "amountOutMin": "990000"
}
Response: {
  "txHash": "0x...",
  "amountOut": "995000"
}
```

### Add Liquidity
```
POST /liquidity
Body: {
  "pool": "0x...",
  "amount0": "1000000",
  "amount1": "2000000"
}
Response: {
  "txHash": "0x...",
  "liquidity": "1414213"
}
```