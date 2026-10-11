package main

import (
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func main() {
	rpcURL := "http://localhost:8545"
	if len(os.Args) > 1 {
		rpcURL = os.Args[1]
	}

	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		fmt.Printf("Failed to connect to RPC: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	blockNumber, err := client.BlockNumber(nil)
	if err != nil {
		fmt.Printf("Failed to get block number: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connected to Ethermint at block %d\n", blockNumber)

	chainID, err := client.ChainID(nil)
	if err != nil {
		fmt.Printf("Failed to get chain ID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Chain ID: %s\n", chainID.String())

	fmt.Println("Go backend is ready to interact with the AMM DEX contracts.")
}
