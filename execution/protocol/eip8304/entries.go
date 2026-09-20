package eip8304

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/erigontech/erigon/common"
)

// Decoding errors. A stored table record is rejected, never partially
// trusted, when its entries cannot be decoded exactly.
var (
	ErrUnknownEntryType  = errors.New("eip8304: unknown entry type")
	ErrShortEncodedEntry = errors.New("eip8304: truncated entry encoding")
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

// entryValueSize returns the value width the canonical encoding gives a type,
// which is what makes an encoded entry self-describing: its type fixes its
// length, so a decoder never has to guess a record's layout.
func entryValueSize(t EntryType) (int, bool) {
	switch t {
	case EntryBlock, EntryTransaction, EntryLogTopic0, EntryLogTopic1, EntryLogTopic2, EntryLogTopic3:
		return hashLength, true
	case EntryLogAddress:
		return addressLength, true
	default:
		return 0, false
	}
}

// DecodeEntry decodes one canonically encoded entry and reports how many bytes
// it consumed. It is the inverse of Entry.Encode; anything that does not decode
// exactly (unknown type, truncated input) is an error, so a store can reject a
// partial or corrupt record instead of trusting it.
func DecodeEntry(encoded []byte) (Entry, int, error) {
	if len(encoded) < 2 {
		return Entry{}, 0, fmt.Errorf("%w: %d bytes for entry type", ErrShortEncodedEntry, len(encoded))
	}
	t := EntryType(binary.BigEndian.Uint16(encoded))
	valueSize, ok := entryValueSize(t)
	if !ok {
		return Entry{}, 0, fmt.Errorf("%w: %d", ErrUnknownEntryType, t)
	}
	size := 2 + valueSize + 8
	if t != EntryBlock {
		size += 8
	}
	if len(encoded) < size {
		return Entry{}, 0, fmt.Errorf("%w: %d bytes for a %d byte entry", ErrShortEncodedEntry, len(encoded), size)
	}

	e := Entry{Type: t, valueSize: uint8(valueSize)}
	copy(e.Value[hashLength-valueSize:], encoded[2:2+valueSize])
	e.block = binary.BigEndian.Uint64(encoded[2+valueSize:])
	if t != EntryBlock {
		e.transaction = binary.BigEndian.Uint32(encoded[2+valueSize+8:])
		e.position = binary.BigEndian.Uint32(encoded[2+valueSize+12:])
	}
	return e, size, nil
}

// DecodeEntries decodes exactly `count` consecutive entries and reports the
// number of bytes they occupy. It fails if the input runs out early or holds
// more than count entries, so a caller can require that a record is consumed
// to the byte.
func DecodeEntries(encoded []byte, count uint64) (Entries, int, error) {
	entries := make(Entries, 0, count)
	offset := 0
	for i := uint64(0); i < count; i++ {
		entry, size, err := DecodeEntry(encoded[offset:])
		if err != nil {
			return nil, 0, fmt.Errorf("entry %d of %d: %w", i, count, err)
		}
		entries = append(entries, entry)
		offset += size
	}
	return entries, offset, nil
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
