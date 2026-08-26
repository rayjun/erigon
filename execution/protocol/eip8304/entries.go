package eip8304

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/erigontech/erigon/common"
)

const (
	hashLength    = 32
	addressLength = 20
)

type EntryType uint16

const (
	EntryBlock EntryType = iota
	EntryTransaction
	EntryLogAddress
	EntryLogTopic0
	EntryLogTopic1
	EntryLogTopic2
	EntryLogTopic3
)

type Entry struct {
	Type        EntryType
	Value       common.Hash
	block       uint64
	transaction uint32
	position    uint32
	valueSize   uint8
}

type EncodedEntry []byte

func (e EncodedEntry) String() string {
	return hex.EncodeToString(e)
}

func (e EncodedEntry) LeafHash() common.Hash {
	return sha256.Sum256(e)
}

func NewBlockEntry(block uint64, blockHash common.Hash) Entry {
	return Entry{Type: EntryBlock, Value: blockHash, block: block, valueSize: hashLength}
}

func NewTransactionEntry(block uint64, transaction, cumulativeLogCount uint32, transactionHash common.Hash) Entry {
	return Entry{
		Type:        EntryTransaction,
		Value:       transactionHash,
		block:       block,
		transaction: transaction,
		position:    cumulativeLogCount,
		valueSize:   hashLength,
	}
}

func NewLogAddressEntry(block uint64, transaction, logIndex uint32, address common.Address) Entry {
	var value common.Hash
	copy(value[hashLength-addressLength:], address[:])
	return Entry{
		Type:        EntryLogAddress,
		Value:       value,
		block:       block,
		transaction: transaction,
		position:    logIndex,
		valueSize:   addressLength,
	}
}

func NewLogTopicEntry(block uint64, transaction, logIndex uint32, topicPosition uint8, topic common.Hash) (Entry, error) {
	if topicPosition > 3 {
		return Entry{}, fmt.Errorf("invalid topic position %d", topicPosition)
	}
	return Entry{
		Type:        EntryLogTopic0 + EntryType(topicPosition),
		Value:       topic,
		block:       block,
		transaction: transaction,
		position:    logIndex,
		valueSize:   hashLength,
	}, nil
}

func (e Entry) Encode() EncodedEntry {
	size := 2 + int(e.valueSize) + 8
	if e.Type != EntryBlock {
		size += 8
	}
	encoded := make(EncodedEntry, size)
	binary.BigEndian.PutUint16(encoded, uint16(e.Type))
	copy(encoded[2:2+e.valueSize], e.Value[hashLength-int(e.valueSize):])
	binary.BigEndian.PutUint64(encoded[2+e.valueSize:], e.block)
	if e.Type == EntryBlock {
		return encoded
	}
	binary.BigEndian.PutUint32(encoded[2+e.valueSize+8:], e.transaction)
	binary.BigEndian.PutUint32(encoded[2+e.valueSize+12:], e.position)
	return encoded
}

type Entries []Entry

// Sort orders entries by their canonical encoded representation, the same key
// the protocol uses for lexicographic table order. Each entry is encoded
// exactly once; the sort itself compares precomputed keys instead of
// re-encoding on every comparison.
func (es Entries) Sort() {
	if len(es) < 2 {
		return
	}
	type keyedEntry struct {
		entry Entry
		key   []byte
	}
	keyed := make([]keyedEntry, len(es))
	for i, e := range es {
		keyed[i] = keyedEntry{entry: e, key: e.Encode()}
	}
	slices.SortStableFunc(keyed, func(a, b keyedEntry) int {
		return bytes.Compare(a.key, b.key)
	})
	for i := range keyed {
		es[i] = keyed[i].entry
	}
}
