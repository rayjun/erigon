package eip8304

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
)

func TestBlockEntryEncodingUsesBigEndianBlockNumber(t *testing.T) {
	entry := NewBlockEntry(40, common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65"))

	require.Equal(t,
		"000042f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b650000000000000028",
		entry.Encode().String(),
	)
}

func TestTransactionEntryEncodingUsesCumulativeLogCount(t *testing.T) {
	entry := NewTransactionEntry(
		42,
		1,
		2,
		common.HexToHash("0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
	)

	require.Equal(t,
		"00010123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef000000000000002a0000000100000002",
		entry.Encode().String(),
	)
}

func TestLogEntryEncodingUsesReceiptLocalLogIndex(t *testing.T) {
	addressEntry := NewLogAddressEntry(42, 1, 2, common.HexToAddress("0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"))

	require.Equal(t,
		"0002c02aaa39b223fe8d0a0e5c4f27ead9083c756cc2000000000000002a0000000100000002",
		addressEntry.Encode().String(),
	)

	for topicIndex := range 4 {
		topicPosition := uint8(topicIndex)
		topicEntry, err := NewLogTopicEntry(
			42,
			1,
			2,
			topicPosition,
			common.HexToHash("0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		)
		require.NoError(t, err)
		require.Equal(t, EntryLogTopic0+EntryType(topicPosition), topicEntry.Type)
		require.Equal(t,
			"000"+string(rune('3'+topicPosition))+"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef000000000000002a0000000100000002",
			topicEntry.Encode().String(),
		)
	}
}

func TestLogTopicEntryRejectsOutOfRangeTopicPosition(t *testing.T) {
	_, err := NewLogTopicEntry(42, 1, 2, 4, common.Hash{})

	require.Error(t, err)
}

func TestEntriesSortByEncodedBytes(t *testing.T) {
	entries := Entries{
		NewBlockEntry(42, common.HexToHash("0xbf98e6cb26f6ff312586968d1f343a3d3c439a8c5c86233aff2a82f1a68263df")),
		NewBlockEntry(40, common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65")),
		NewLogAddressEntry(42, 0, 0, common.HexToAddress("0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2")),
	}

	entries.Sort()

	require.Equal(t, EntryBlock, entries[0].Type)
	require.Equal(t, "0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65", entries[0].Value.String())
	require.Equal(t, EntryBlock, entries[1].Type)
	require.Equal(t, EntryLogAddress, entries[2].Type)
}

func TestEncodedEntryLeafHashUsesSHA256(t *testing.T) {
	entry := NewBlockEntry(40, common.HexToHash("0x42f66a2e9f9c68e223e8d826145d7cfacb00520dba6a9555803121de29790b65"))

	require.Equal(t,
		"0xef9f34bd4316ac2801d63afd42a6e967a928dfe53194cde890a2beb5bba31f05",
		entry.Encode().LeafHash().String(),
	)
}

// Decoding exists so a stored table can be read back exactly; the tests below
// pin it as the inverse of encoding for every entry type, and pin the failures
// a damaged record must produce.
func TestDecodeEntryIsTheInverseOfEncode(t *testing.T) {
	topic := func(position uint8, hash common.Hash) Entry {
		entry, err := NewLogTopicEntry(7, 1, 2, position, hash)
		require.NoError(t, err)
		return entry
	}
	entries := Entries{
		NewBlockEntry(7, common.HexToHash("0xaa")),
		NewTransactionEntry(7, 1, 2, common.HexToHash("0xbb")),
		NewLogAddressEntry(7, 1, 2, common.HexToAddress("0xdeadbeef")),
		topic(0, common.HexToHash("0x01")),
		topic(1, common.HexToHash("0x02")),
		topic(2, common.HexToHash("0x03")),
		topic(3, common.HexToHash("0x04")),
	}

	for _, want := range entries {
		encoded := want.Encode()
		got, size, err := DecodeEntry(encoded)
		require.NoError(t, err)
		require.Equal(t, len(encoded), size)
		require.Equal(t, want, got, "decode must return the entry encode produced (%s)", encoded.String())
	}

	// Decoding a run consumes exactly the bytes the run occupies.
	var all []byte
	for _, entry := range entries {
		all = append(all, entry.Encode()...)
	}
	got, consumed, err := DecodeEntries(all, uint64(len(entries)))
	require.NoError(t, err)
	require.Equal(t, len(all), consumed)
	require.Equal(t, entries, got)
}

func TestDecodeEntryRejectsMalformedInput(t *testing.T) {
	// An unknown tag can never be part of a table.
	unknown := []byte{0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}
	_, _, err := DecodeEntry(unknown)
	require.ErrorIs(t, err, ErrUnknownEntryType)

	block := NewBlockEntry(42, testHash(1)).Encode()
	transaction := NewTransactionEntry(42, 0, 0, testHash(2)).Encode()
	address := NewLogAddressEntry(42, 0, 0, common.HexToAddress("0xdead")).Encode()
	for _, encoded := range [][]byte{block, transaction, address} {
		for cut := 0; cut < len(encoded); cut++ {
			_, _, err := DecodeEntry(encoded[:cut])
			require.ErrorIs(t, err, ErrShortEncodedEntry, "a %d byte prefix must not decode", cut)
		}
	}

	// A run that claims more entries than it holds fails at the missing entry.
	_, _, err = DecodeEntries(block, 2)
	require.ErrorIs(t, err, ErrShortEncodedEntry)
	// Decoding fewer entries than the run holds leaves the remainder visible to
	// the caller, which is what makes trailing bytes detectable.
	_, consumed, err := DecodeEntries(append(append([]byte{}, block...), transaction...), 1)
	require.NoError(t, err)
	require.Equal(t, len(block), consumed)
}
