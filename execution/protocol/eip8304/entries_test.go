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

	for topicPosition := uint8(0); topicPosition < 4; topicPosition++ {
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
