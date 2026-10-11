package services

import (
	"math/big"
)

// BlockchainAdapter defines the interface for interacting with blockchain networks
type BlockchainAdapter interface {
	// GetBlockNumber returns the current block number
	GetBlockNumber() (uint64, error)

	// GetTransactionReceipt returns the receipt of a transaction
	GetTransactionReceipt(txHash string) (*Receipt, error)

	// CallContract executes a read-only call against a contract
	CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error)

	// SendTransaction sends a transaction to the blockchain
	SendTransaction(tx *Transaction) (string, error)

	// GetLogs returns logs matching an address/topics filter in [fromBlock, toBlock].
	GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error)
}

// Receipt represents a transaction receipt from the blockchain
type Receipt struct {
	Status            uint64   `json:"status"`            // 0 = failure, 1 = success
	BlockNumber       uint64   `json:"blockNumber"`
	BlockHash         string   `json:"blockHash"`
	TxHash            string   `json:"txHash"`
	GasUsed           uint64   `json:"gasUsed"`
	ContractAddress   string   `json:"contractAddress"`
	Logs              []*Log   `json:"logs"`
	CumulativeGasUsed uint64   `json:"cumulativeGasUsed"`
}

// Log represents an event log emitted by a smart contract
type Log struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        []byte   `json:"data"`
	BlockNumber uint64   `json:"blockNumber"`
	TxHash      string   `json:"txHash"`
	LogIndex    uint     `json:"logIndex"`
}

// Transaction represents a blockchain transaction
type Transaction struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Value    *big.Int `json:"value"`
	Data     []byte   `json:"data"`
	GasLimit uint64   `json:"gasLimit"`
	GasPrice *big.Int `json:"gasPrice"`
	Nonce    uint64   `json:"nonce"`
	ChainID  *big.Int `json:"chainID"`
}

// MockBlockchainAdapter is a mock implementation of BlockchainAdapter for testing
type MockBlockchainAdapter struct {
	BlockNumber       uint64
	Receipts          map[string]*Receipt
	ContractResponses map[string][]byte
	SendTxHash        string
	Error             error
	Logs              []Log
	Sent              []*Transaction
}

// NewMockBlockchainAdapter creates a new MockBlockchainAdapter with default values
func NewMockBlockchainAdapter() *MockBlockchainAdapter {
	return &MockBlockchainAdapter{
		BlockNumber:       12345,
		Receipts:          make(map[string]*Receipt),
		ContractResponses: make(map[string][]byte),
		SendTxHash:        "0xabc123def45678900000000000000000000000000000000000000000000000000",
	}
}

// GetBlockNumber returns the mock current block number
func (m *MockBlockchainAdapter) GetBlockNumber() (uint64, error) {
	if m.Error != nil {
		return 0, m.Error
	}
	return m.BlockNumber, nil
}

// GetTransactionReceipt returns a mock transaction receipt
func (m *MockBlockchainAdapter) GetTransactionReceipt(txHash string) (*Receipt, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	receipt, ok := m.Receipts[txHash]
	if !ok {
		return nil, nil
	}
	return receipt, nil
}

// CallContract returns a mock contract call response
func (m *MockBlockchainAdapter) CallContract(contract string, data []byte, blockNumber uint64) ([]byte, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	response, ok := m.ContractResponses[contract]
	if !ok {
		// Return mock response data
		return []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00}, nil
	}
	return response, nil
}

// SendTransaction sends a mock transaction
func (m *MockBlockchainAdapter) SendTransaction(tx *Transaction) (string, error) {
	if m.Error != nil {
		return "", m.Error
	}
	m.Sent = append(m.Sent, tx)
	return m.SendTxHash, nil
}

// GetLogs returns mock logs whose block number is inside [fromBlock, toBlock].
func (m *MockBlockchainAdapter) GetLogs(fromBlock, toBlock uint64, address string, topics [][]string) ([]Log, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	var out []Log
	for _, l := range m.Logs {
		if l.BlockNumber < fromBlock || l.BlockNumber > toBlock {
			continue
		}
		if address != "" && l.Address != address {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
