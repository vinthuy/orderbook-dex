package services

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// EVMAdapter is a real BlockchainAdapter backed by an Ethereum JSON-RPC node.
type EVMAdapter struct {
	client  *ethclient.Client
	key     *ecdsa.PrivateKey
	address common.Address
	chainID *big.Int
}

func NewEVMAdapter(rpcURL, privateKeyHex string, chainID int64) (*EVMAdapter, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	key, err := crypto.HexToECDSA(trim0x(privateKeyHex))
	if err != nil {
		return nil, fmt.Errorf("parse operator key: %w", err)
	}
	return &EVMAdapter{
		client:  client,
		key:     key,
		address: crypto.PubkeyToAddress(key.PublicKey),
		chainID: big.NewInt(chainID),
	}, nil
}

func trim0x(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

func (a *EVMAdapter) GetBlockNumber() (uint64, error) {
	return a.client.BlockNumber(context.Background())
}

func (a *EVMAdapter) GetTransactionReceipt(txHash string) (*Receipt, error) {
	r, err := a.client.TransactionReceipt(context.Background(), common.HexToHash(txHash))
	if err != nil {
		return nil, err
	}
	out := &Receipt{
		Status:            r.Status,
		BlockNumber:       r.BlockNumber.Uint64(),
		BlockHash:         r.BlockHash.Hex(),
		TxHash:            r.TxHash.Hex(),
		GasUsed:           r.GasUsed,
		ContractAddress:   r.ContractAddress.Hex(),
		CumulativeGasUsed: r.CumulativeGasUsed,
	}
	for _, l := range r.Logs {
		converted := toLog(*l)
		out.Logs = append(out.Logs, &converted)
	}
	return out, nil
}

func (a *EVMAdapter) CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error) {
	msg := ethereum.CallMsg{To: ptrAddr(common.HexToAddress(contract)), Data: data}
	var bn *big.Int
	if blockNumber > 0 {
		bn = new(big.Int).SetUint64(blockNumber)
	}
	return a.client.CallContract(context.Background(), msg, bn)
}

func (a *EVMAdapter) SendTransaction(tx *Transaction) (string, error) {
	ctx := context.Background()
	nonce, err := a.client.PendingNonceAt(ctx, a.address)
	if err != nil {
		return "", err
	}
	gasPrice, err := a.client.SuggestGasPrice(ctx)
	if err != nil {
		return "", err
	}
	gasLimit := tx.GasLimit
	if gasLimit == 0 {
		gasLimit = 1_000_000
	}
	value := tx.Value
	if value == nil {
		value = big.NewInt(0)
	}
	raw := types.NewTransaction(nonce, common.HexToAddress(tx.To), value, gasLimit, gasPrice, tx.Data)
	signed, err := types.SignTx(raw, types.LatestSignerForChainID(a.chainID), a.key)
	if err != nil {
		return "", err
	}
	if err := a.client.SendTransaction(ctx, signed); err != nil {
		return "", err
	}
	return signed.Hash().Hex(), nil
}

func (a *EVMAdapter) GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error) {
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
	}
	if address != "" {
		q.Addresses = []common.Address{common.HexToAddress(address)}
	}
	for _, group := range topics {
		var hs []common.Hash
		for _, h := range group {
			hs = append(hs, common.HexToHash(h))
		}
		q.Topics = append(q.Topics, hs)
	}
	logs, err := a.client.FilterLogs(context.Background(), q)
	if err != nil {
		return nil, err
	}
	out := make([]Log, 0, len(logs))
	for _, l := range logs {
		out = append(out, toLog(l))
	}
	return out, nil
}

func toLog(l types.Log) Log {
	topics := make([]string, len(l.Topics))
	for i, t := range l.Topics {
		topics[i] = t.Hex()
	}
	return Log{
		Address:     l.Address.Hex(),
		Topics:      topics,
		Data:        l.Data,
		BlockNumber: l.BlockNumber,
		TxHash:      l.TxHash.Hex(),
		LogIndex:    l.Index,
	}
}

func ptrAddr(a common.Address) *common.Address { return &a }
