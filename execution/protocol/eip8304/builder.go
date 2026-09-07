package eip8304

import (
	"errors"
	"fmt"
	"math"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/types"
)

var (
	ErrReceiptCountMismatch = errors.New("transaction and receipt counts differ")
	ErrNilTransaction       = errors.New("nil transaction")
	ErrNilReceipt           = errors.New("nil receipt")
	ErrNilLog               = errors.New("nil log")
	ErrTooManyTransactions  = errors.New("too many transactions")
	ErrTooManyLogs          = errors.New("too many logs")
	ErrTooManyLogTopics     = errors.New("too many log topics")
	ErrMissingTxHash        = errors.New("receipt has no transaction hash")
)

// BuildBlockEntriesFromReceipts builds the block's entries from its receipts
// alone, using each receipt's TxHash (filled by DeriveFields* on the caller
// side) as the ordered transaction hash. It fails closed when an expected
// TxHash is missing, so a receipt that has not been derived cannot silently
// produce a table with zero transaction hashes.
func BuildBlockEntriesFromReceipts(
	block uint64,
	parentBlockHash common.Hash,
	receipts types.Receipts,
) (Entries, error) {
	if len(receipts) > math.MaxUint32 {
		return nil, ErrTooManyTransactions
	}
	transactionHashes := make([]common.Hash, len(receipts))
	for i, receipt := range receipts {
		if receipt == nil {
			return nil, fmt.Errorf("%w at transaction %d", ErrNilReceipt, i)
		}
		if receipt.TxHash == (common.Hash{}) {
			return nil, fmt.Errorf("%w at transaction %d", ErrMissingTxHash, i)
		}
		transactionHashes[i] = receipt.TxHash
	}
	return buildBlockEntriesFromHashes(block, parentBlockHash, transactionHashes, receipts)
}

// BuildBlockEntries returns entries in block execution order; callers must sort them before table hashing.
func BuildBlockEntries(
	block uint64,
	parentBlockHash common.Hash,
	transactions types.Transactions,
	receipts types.Receipts,
) (Entries, error) {
	if len(transactions) != len(receipts) {
		return nil, ErrReceiptCountMismatch
	}
	if len(transactions) > math.MaxUint32 {
		return nil, ErrTooManyTransactions
	}

	transactionHashes := make([]common.Hash, len(transactions))
	for i, transaction := range transactions {
		if transaction == nil {
			return nil, fmt.Errorf("%w at index %d", ErrNilTransaction, i)
		}
		transactionHashes[i] = transaction.Hash()
	}
	return buildBlockEntriesFromHashes(block, parentBlockHash, transactionHashes, receipts)
}

func buildBlockEntriesFromHashes(
	block uint64,
	parentBlockHash common.Hash,
	transactionHashes []common.Hash,
	receipts types.Receipts,
) (Entries, error) {
	if len(transactionHashes) != len(receipts) {
		return nil, ErrReceiptCountMismatch
	}
	if len(transactionHashes) > math.MaxUint32 {
		return nil, ErrTooManyTransactions
	}

	entryCount := len(transactionHashes)
	if block > 0 {
		entryCount++
	}
	var cumulativeLogCount uint32
	for transactionIndex, receipt := range receipts {
		if receipt == nil {
			return nil, fmt.Errorf("%w at transaction %d", ErrNilReceipt, transactionIndex)
		}
		if uint64(cumulativeLogCount)+uint64(len(receipt.Logs)) > math.MaxUint32 {
			return nil, ErrTooManyLogs
		}
		for logIndex, log := range receipt.Logs {
			if log == nil {
				return nil, fmt.Errorf("%w at transaction %d log %d", ErrNilLog, transactionIndex, logIndex)
			}
			if len(log.Topics) > 4 {
				return nil, fmt.Errorf("%w at transaction %d log %d", ErrTooManyLogTopics, transactionIndex, logIndex)
			}
			entryCount += 1 + len(log.Topics)
		}
		cumulativeLogCount += uint32(len(receipt.Logs))
	}

	entries := make(Entries, 0, entryCount)
	if block > 0 {
		entries = append(entries, NewBlockEntry(block-1, parentBlockHash))
	}
	cumulativeLogCount = 0
	for transactionIndex, transactionHash := range transactionHashes {
		entries = append(entries, NewTransactionEntry(block, uint32(transactionIndex), cumulativeLogCount, transactionHash))
		for logIndex, log := range receipts[transactionIndex].Logs {
			entries = append(entries, NewLogAddressEntry(block, uint32(transactionIndex), uint32(logIndex), log.Address))
			for topicPosition, topic := range log.Topics {
				entry, err := NewLogTopicEntry(block, uint32(transactionIndex), uint32(logIndex), uint8(topicPosition), topic)
				if err != nil {
					return nil, err
				}
				entries = append(entries, entry)
			}
		}
		cumulativeLogCount += uint32(len(receipts[transactionIndex].Logs))
	}
	return entries, nil
}
