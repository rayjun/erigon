package eip8304

import (
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// FinalizeL0 builds the sorted L0 entries for a block from its receipts,
// computes the SSZ list root under the given limit, and applies the index
// contract update for the block's level-0 table (firstBlock = block,
// tableSize = 1). The index address and the SSZ list limit are explicit
// arguments; no unconfirmed value is defaulted.
func FinalizeL0(
	block uint64,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	indexAddr accounts.Address,
	listLimit uint64,
	syscall rules.SystemCall,
) error {
	entries, err := BuildBlockEntriesFromReceipts(block, parentBlockHash, receipts)
	if err != nil {
		return err
	}
	entries.Sort()

	leaves := make([]common.Hash, len(entries))
	for i, e := range entries {
		leaves[i] = e.Encode().LeafHash()
	}
	root, err := ListHashRoot(leaves, listLimit)
	if err != nil {
		return err
	}

	table := L0(block)
	return misc.ApplyIndexEip8304(indexAddr, table.FirstBlock, table.TableSize, root, syscall)
}
