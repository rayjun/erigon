package eip8304

import (
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/execution/protocol/misc"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
)

// FinalizeL0 runs the whole level-0 side of the EIP-8304 finalization for one
// block: build the sorted L0 entries from the block's receipts, compute the
// SSZ list root under the given limit, and apply the index contract update
// (firstBlock = block, tableSize = 1). The index address and the SSZ list
// limit are explicit arguments — no unconfirmed value is defaulted.
//
// It is the composition the shared Finalize paths will call once the
// integration points are confirmed; it does not itself touch Finalize, so
// default networks are unaffected until the caller wires it behind
// config.IsEip8304.
func FinalizeL0(
	block uint64,
	parentBlockHash common.Hash,
	receipts types.Receipts,
	indexAddr accounts.Address,
	listLimit uint64,
	syscall func(addr accounts.Address, data []byte) ([]byte, error),
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

	return misc.ApplyIndexEip8304(indexAddr, block, 1, root, syscall)
}
