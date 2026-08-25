package eip8304

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	mergeFixtureSchema      = "eip-8304-merge-fixture"
	mergeFixtureVersion     = 1
	mergeFixtureStatus      = "draft-pre-root"
	mergeFixtureEIPRevision = "81b976ac01591fed2eecb73fa574f27cd18db2e8"
)

type mergeFixture struct {
	Schema        string             `json:"schema"`
	SchemaVersion uint64             `json:"schema_version"`
	Status        string             `json:"status"`
	EIPRevision   string             `json:"eip_revision"`
	Name          string             `json:"name"`
	Blocks        []fixtureInput     `json:"blocks"`
	Expected      mergeFixtureOutput `json:"expected"`
}

type mergeFixtureOutput struct {
	PerBlockL0       []mergeL0Output `json:"per_block_l0"`
	MergedSorted     []string        `json:"merged_sorted"`
	MergedLeafHashes []string        `json:"merged_leaf_hashes"`
	MergedEntryCount string          `json:"merged_entry_count"`
}

type mergeL0Output struct {
	EncodedEntriesSorted []string `json:"encoded_entries_sorted"`
	EntryCount           string   `json:"entry_count"`
}

// TestMergeFixtures verifies four-way merge against cross-implementation
// vectors: each block's sorted L0 table is recomputed from the production
// builder, then the four (in this fixture) tables are merged. The merged
// sorted entries and their leaf hashes must match the fixture exactly,
// exposing the intermediate per-block and merged values.
func TestMergeFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/merge/v1/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f := readMergeFixture(t, path)
			require.Equal(t, mergeFixtureSchema, f.Schema)
			require.Equal(t, uint64(mergeFixtureVersion), f.SchemaVersion)
			require.Equal(t, mergeFixtureStatus, f.Status)
			require.Equal(t, mergeFixtureEIPRevision, f.EIPRevision)
			require.NotEmpty(t, f.Name)
			require.Len(t, f.Blocks, 4, "merge fixture must cover exactly four L0 tables")

			var l0 []Entries
			var perBlock []mergeL0Output
			for _, input := range f.Blocks {
				block, parentHash, transactionHashes, receipts, errorCode := decodeBlock(input)
				require.Empty(t, errorCode)

				entries, err := buildBlockEntriesFromHashes(block, parentHash, transactionHashes, receipts)
				require.NoError(t, err)
				entries.Sort()

				perBlock = append(perBlock, mergeL0Output{
					EncodedEntriesSorted: encodeEntries(entries),
					EntryCount:           fmt.Sprintf("0x%x", len(entries)),
				})
				l0 = append(l0, entries)
			}

			require.Equal(t, f.Expected.PerBlockL0, perBlock)

			merged := MergeSorted(l0...)
			require.Equal(t, f.Expected.MergedSorted, encodeEntries(merged))
			require.Equal(t, f.Expected.MergedEntryCount, fmt.Sprintf("0x%x", len(merged)))

			leaves := make([]string, len(merged))
			for i, e := range merged {
				leaves[i] = e.Encode().LeafHash().String()
			}
			require.Equal(t, f.Expected.MergedLeafHashes, leaves)
			require.True(t, isSorted(merged))
		})
	}
}

func readMergeFixture(t *testing.T, path string) mergeFixture {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var f mergeFixture
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&f))
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		require.Fail(t, "fixture must contain a single JSON value")
	}
	return f
}
