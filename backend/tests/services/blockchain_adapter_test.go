package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlockchainAdapter_GetBlockNumber(t *testing.T) {
	adapter := NewMockBlockchainAdapter()

	blockNumber, err := adapter.GetBlockNumber()

	assert.NoError(t, err)
	assert.Greater(t, blockNumber, uint64(0))
}

func TestBlockchainAdapter_CallContract(t *testing.T) {
	adapter := NewMockBlockchainAdapter()

	result, err := adapter.CallContract(
		"0x1234567890123456789012345678901234567890",
		[]byte{0x01, 0x02, 0x03},
		100,
	)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}
