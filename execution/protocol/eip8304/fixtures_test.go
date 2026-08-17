package eip8304

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/hexutil"
	"github.com/erigontech/erigon/execution/types"
)

const (
	fixtureSchema      = "eip-8304-fixture"
	fixtureVersion     = 1
	fixtureStatus      = "draft-pre-root"
	fixtureEIPRevision = "81b976ac01591fed2eecb73fa574f27cd18db2e8"
)

type fixture struct {
	Schema        string         `json:"schema"`
	SchemaVersion uint64         `json:"schema_version"`
	Status        string         `json:"status"`
	EIPRevision   string         `json:"eip_revision"`
	Name          string         `json:"name"`
	Input         fixtureInput   `json:"input"`
	Expected      *fixtureOutput `json:"expected,omitempty"`
	ExpectedError string         `json:"expected_error,omitempty"`
}

type fixtureInput struct {
	BlockNumber     string               `json:"block_number"`
	ParentBlockHash string               `json:"parent_block_hash"`
	Transactions    []fixtureTransaction `json:"transactions"`
	Receipts        []fixtureReceipt     `json:"receipts"`
}

type fixtureTransaction struct {
	Hash string `json:"hash"`
}

type fixtureReceipt struct {
	Logs []fixtureLog `json:"logs"`
}

type fixtureLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
}

type fixtureOutput struct {
	EncodedEntriesChronological []string `json:"encoded_entries_chronological"`
	EncodedEntriesSorted        []string `json:"encoded_entries_sorted"`
	LeafHashesSorted            []string `json:"leaf_hashes_sorted"`
	EntryCount                  string   `json:"entry_count"`
}

func TestFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/v1/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f := readFixture(t, path)
			require.Equal(t, fixtureSchema, f.Schema)
			require.Equal(t, uint64(fixtureVersion), f.SchemaVersion)
			require.Equal(t, fixtureStatus, f.Status)
			require.Equal(t, fixtureEIPRevision, f.EIPRevision)
			require.NotEmpty(t, f.Name)
			require.NotEqual(t, f.Expected != nil, f.ExpectedError != "")

			actual, errorCode := calculateFixture(f.Input)
			if f.ExpectedError != "" {
				require.Equal(t, f.ExpectedError, errorCode)
				return
			}

			require.Empty(t, errorCode)
			require.Equal(t, *f.Expected, actual)
		})
	}
}

func readFixture(t *testing.T, path string) fixture {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var f fixture
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&f))
	require.NoError(t, ensureJSONEOF(decoder))
	return f
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}

func calculateFixture(input fixtureInput) (fixtureOutput, string) {
	block, err := hexutil.DecodeUint64(input.BlockNumber)
	if err != nil {
		return fixtureOutput{}, "invalid_block_number"
	}
	parentHash, err := decodeHash(input.ParentBlockHash)
	if err != nil {
		return fixtureOutput{}, "invalid_parent_block_hash"
	}
	transactionHashes := make([]common.Hash, len(input.Transactions))
	for transactionIndex, transaction := range input.Transactions {
		transactionHash, err := decodeHash(transaction.Hash)
		if err != nil {
			return fixtureOutput{}, "invalid_transaction_hash"
		}
		transactionHashes[transactionIndex] = transactionHash
	}
	receipts := make(types.Receipts, len(input.Receipts))
	for receiptIndex, fixtureReceipt := range input.Receipts {
		receipt := &types.Receipt{Logs: make(types.Logs, len(fixtureReceipt.Logs))}
		for logIndex, fixtureLog := range fixtureReceipt.Logs {
			log := &types.Log{Topics: make([]common.Hash, len(fixtureLog.Topics))}
			address, err := decodeAddress(fixtureLog.Address)
			if err != nil {
				return fixtureOutput{}, "invalid_log_address"
			}
			log.Address = address
			for topicPosition, encodedTopic := range fixtureLog.Topics {
				topic, err := decodeHash(encodedTopic)
				if err != nil {
					return fixtureOutput{}, "invalid_log_topic"
				}
				log.Topics[topicPosition] = topic
			}
			receipt.Logs[logIndex] = log
		}
		receipts[receiptIndex] = receipt
	}

	entries, err := buildBlockEntriesFromHashes(block, parentHash, transactionHashes, receipts)
	if err != nil {
		switch {
		case errors.Is(err, ErrReceiptCountMismatch):
			return fixtureOutput{}, "transaction_receipt_count_mismatch"
		case errors.Is(err, ErrTooManyTransactions):
			return fixtureOutput{}, "too_many_transactions"
		case errors.Is(err, ErrTooManyLogs):
			return fixtureOutput{}, "too_many_logs"
		case errors.Is(err, ErrTooManyLogTopics):
			return fixtureOutput{}, "too_many_log_topics"
		default:
			return fixtureOutput{}, "unexpected_builder_error"
		}
	}

	chronological := encodeEntries(entries)
	sortedEntries := slices.Clone(entries)
	sortedEntries.Sort()
	sorted := encodeEntries(sortedEntries)
	leaves := make([]string, len(sortedEntries))
	for i, entry := range sortedEntries {
		leaves[i] = entry.Encode().LeafHash().String()
	}
	return fixtureOutput{
		EncodedEntriesChronological: chronological,
		EncodedEntriesSorted:        sorted,
		LeafHashesSorted:            leaves,
		EntryCount:                  fmt.Sprintf("0x%x", len(entries)),
	}, ""
}

func TestCalculateFixtureRejectsMalformedFields(t *testing.T) {
	validHash := "0x" + strings.Repeat("00", hashLength)
	validAddress := "0x" + strings.Repeat("00", addressLength)
	tests := []struct {
		name  string
		input fixtureInput
		want  string
	}{
		{
			name: "block number",
			input: fixtureInput{
				BlockNumber: "42", ParentBlockHash: validHash,
			},
			want: "invalid_block_number",
		},
		{
			name: "parent block hash",
			input: fixtureInput{
				BlockNumber: "0x2a", ParentBlockHash: "0x01",
			},
			want: "invalid_parent_block_hash",
		},
		{
			name: "transaction hash",
			input: fixtureInput{
				BlockNumber: "0x2a", ParentBlockHash: validHash,
				Transactions: []fixtureTransaction{{Hash: "0x01"}}, Receipts: []fixtureReceipt{{}},
			},
			want: "invalid_transaction_hash",
		},
		{
			name: "log address",
			input: fixtureInput{
				BlockNumber: "0x2a", ParentBlockHash: validHash,
				Transactions: []fixtureTransaction{{Hash: validHash}},
				Receipts:     []fixtureReceipt{{Logs: []fixtureLog{{Address: "0x01"}}}},
			},
			want: "invalid_log_address",
		},
		{
			name: "log topic",
			input: fixtureInput{
				BlockNumber: "0x2a", ParentBlockHash: validHash,
				Transactions: []fixtureTransaction{{Hash: validHash}},
				Receipts:     []fixtureReceipt{{Logs: []fixtureLog{{Address: validAddress, Topics: []string{"0x01"}}}}},
			},
			want: "invalid_log_topic",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, errorCode := calculateFixture(test.input)
			require.Equal(t, test.want, errorCode)
		})
	}
}

func encodeEntries(entries Entries) []string {
	encoded := make([]string, len(entries))
	for i, entry := range entries {
		encoded[i] = "0x" + entry.Encode().String()
	}
	return encoded
}

func decodeHash(input string) (common.Hash, error) {
	decoded, err := hexutil.Decode(input)
	if err != nil || len(decoded) != hashLength {
		return common.Hash{}, errors.New("invalid hash")
	}
	return common.BytesToHash(decoded), nil
}

func decodeAddress(input string) (common.Address, error) {
	decoded, err := hexutil.Decode(input)
	if err != nil || len(decoded) != addressLength {
		return common.Address{}, errors.New("invalid address")
	}
	return common.BytesToAddress(decoded), nil
}
