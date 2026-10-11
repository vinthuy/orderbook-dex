package services

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"web3-amm-dex-backend/models"
)

var (
	client *ethclient.Client
	rpcURL string = "http://localhost:8545"
	mu     sync.RWMutex
)

func SetRPCURL(url string) {
	rpcURL = url
}

var events = []models.PoolCreatedEvent{}
var eventMu sync.RWMutex

func InitClient() error {
	var err error
	client, err = ethclient.Dial(rpcURL)
	if err != nil {
		return fmt.Errorf("failed to connect to Ethereum node: %w", err)
	}
	blockNumber, err := client.BlockNumber(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get block number: %w", err)
	}
	fmt.Printf("Connected to Ethereum node, current block: %d\n", blockNumber)
	return nil
}

func GetClient() *ethclient.Client {
	mu.RLock()
	defer mu.RUnlock()
	return client
}

func GetPoolInfo(poolAddr string) (*models.PoolInfo, error) {
	c := GetClient()
	defer c.Close()

	pool := common.HexToAddress(poolAddr)
	block, err := c.BlockByNumber(context.Background(), nil)
	if err != nil {
		return nil, err
	}

	code, err := c.CodeAt(context.Background(), pool, block.Number())
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("contract not found at %s", poolAddr)
	}

	return &models.PoolInfo{
		Token0:      poolAddr,
		Token1:      poolAddr,
		Reserve0:    "0",
		Reserve1:    "0",
		TotalSupply: "0",
	}, nil
}

func GetAllPools() ([]models.PoolInfo, error) {
	return []models.PoolInfo{}, nil
}

func Swap(tokenIn, tokenOut, amountIn, amountOutMin string) (*models.SwapResponse, error) {
	c := GetClient()
	defer c.Close()

	chainID, err := c.ChainID(context.Background())
	if err != nil {
		return nil, err
	}

	amount := new(big.Int)
	amount.SetString(amountIn, 10)

	to := common.HexToAddress("0x0000000000000000000000000000000000000000")
	nonce, _ := c.PendingNonceAt(context.Background(), to)

	fmt.Printf("Swap: %s -> %s, Amount: %s\n", tokenIn, tokenOut, amountIn)
	fmt.Printf("Chain ID: %s, Nonce: %d\n", chainID.String(), nonce)

	return &models.SwapResponse{
		AmountIn:  amountIn,
		AmountOut: "0",
		TokenIn:   tokenIn,
		TokenOut:  tokenOut,
		Pool:      "",
	}, nil
}

func AddLiquidity(token0Addr, token1Addr, amount0, amount1 string) (*models.LiquidityResponse, error) {
	c := GetClient()
	defer c.Close()

	amount0Big := new(big.Int)
	amount0Big.SetString(amount0, 10)
	amount1Big := new(big.Int)
	amount1Big.SetString(amount1, 10)

	fmt.Printf("Add Liquidity: Token0=%s, Token1=%s, Amount0=%s, Amount1=%s\n",
		token0Addr, token1Addr, amount0, amount1)

	return &models.LiquidityResponse{
		Liquidity: "0",
	}, nil
}

func RemoveLiquidity(token0, token1, liquidity string) (*models.LiquidityResponse, error) {
	c := GetClient()
	defer c.Close()

	fmt.Printf("Remove Liquidity: Token0=%s, Token1=%s, Liquidity=%s\n", token0, token1, liquidity)

	return &models.LiquidityResponse{}, nil
}

func GetRecentEvents(limit int) ([]models.PoolCreatedEvent, error) {
	eventMu.RLock()
	defer eventMu.RUnlock()

	if len(events) < limit {
		return events, nil
	}
	return events[len(events)-limit:], nil
}

func ListenEvents() {
	c := GetClient()
	defer c.Close()

	q := make(chan types.Log)
	sub, err := c.SubscribeFilterLogs(context.Background(), ethereum.FilterQuery{}, q)
	if err != nil {
		fmt.Printf("Failed to subscribe logs: %v\n", err)
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case log, ok := <-q:
			if !ok {
				return
			}
			eventMu.Lock()
			events = append(events, models.PoolCreatedEvent{
				Pool:   log.Address.Hex(),
				Block:  log.BlockNumber,
				TxHash: log.TxHash.Hex(),
			})
			eventMu.Unlock()
			fmt.Printf("Event: Pool=%s Block=%d Tx=%s\n",
				log.Address.Hex(), log.BlockNumber, log.TxHash.Hex())
		case <-time.After(30 * time.Second):
			return
		}
	}
}

func GetCurrentBlock() (uint64, error) {
	c := GetClient()
	defer c.Close()
	return c.BlockNumber(context.Background())
}

func GetBalance(addr string) (*big.Int, error) {
	c := GetClient()
	defer c.Close()
	return c.BalanceAt(context.Background(), common.HexToAddress(addr), nil)
}

func GetCode(addr string) ([]byte, error) {
	c := GetClient()
	defer c.Close()
	return c.CodeAt(context.Background(), common.HexToAddress(addr), nil)
}

func WaitForTransaction(txHash string) (*types.Receipt, error) {
	c := GetClient()
	defer c.Close()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for i := 0; i < 120; i++ {
		receipt, err := c.TransactionReceipt(context.Background(), common.HexToHash(txHash))
		if err == nil && receipt != nil {
			return receipt, nil
		}
		<-ticker.C
	}
	return nil, fmt.Errorf("transaction not confirmed after timeout")
}

func SubscribeLogs() (ethereum.Subscription, error) {
	c := GetClient()
	q := ethereum.FilterQuery{}
	logChan := make(chan types.Log)
	return c.SubscribeFilterLogs(context.Background(), q, logChan)
}
