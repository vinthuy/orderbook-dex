package models

type PoolInfo struct {
	Token0     string `json:"token0"`
	Token1     string `json:"token1"`
	Reserve0   string `json:"reserve0"`
	Reserve1   string `json:"reserve1"`
	TotalSupply string `json:"totalSupply"`
}

type SwapRequest struct {
	TokenIn      string `json:"tokenIn"`
	TokenOut     string `json:"tokenOut"`
	AmountIn     string `json:"amountIn"`
	AmountOutMin string `json:"amountOutMin"`
}

type SwapResponse struct {
	AmountIn   string `json:"amountIn"`
	AmountOut  string `json:"amountOut"`
	TokenIn    string `json:"tokenIn"`
	TokenOut   string `json:"tokenOut"`
	Pool       string `json:"pool"`
}

type LiquidityRequest struct {
	Token0Addr string `json:"token0Addr"`
	Token1Addr string `json:"token1Addr"`
	Amount0    string `json:"amount0"`
	Amount1    string `json:"amount1"`
}

type LiquidityResponse struct {
	Liquidity string `json:"liquidity"`
}

type PoolCreatedEvent struct {
	Token0  string `json:"token0"`
	Token1  string `json:"token1"`
	Pool    string `json:"pool"`
	Block   uint64 `json:"block"`
	TxHash  string `json:"txHash"`
}

type SwapEvent struct {
	Sender    string `json:"sender"`
	Amount0In string `json:"amount0In"`
	Amount1In string `json:"amount1In"`
	Amount0Out string `json:"amount0Out"`
	Amount1Out string `json:"amount1Out"`
	Pool      string `json:"pool"`
}

type MintEvent struct {
	To       string `json:"to"`
	Liquidity string `json:"liquidity"`
	Pool     string `json:"pool"`
}
