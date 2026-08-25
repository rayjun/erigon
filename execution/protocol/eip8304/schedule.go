package eip8304

// The protocol-mandated table schedule. A table is identified by
// (first_block, table_size); first_block is always a multiple of table_size
// and each higher level merges the four adjacent tables of the level below
// (common ratio 4).
//
// Level 0 tables (table_size=1) are written immediately at their own block.
// A higher-level table (first_block, table_size) is written, i.e. its root is
// passed to the system index contract, after processing the transactions of
// block first_block + table_size - 1 + table_size/4. This matches the EIP's
// "delay of TABLE_SIZES[i] // 4 blocks".
//
// These helpers are pure schedule arithmetic. They do not depend on the SSZ
// list bound, so they are fully implementable while that parameter is still
// open in the specification lock.
var TableSizes = []uint64{1, 4, 16, 64, 256}

// TablesPerLevel is the ring-buffer length storing table roots per level.
const TablesPerLevel = 1024

// TableRef identifies one index table.
type TableRef struct {
	FirstBlock uint64
	TableSize  uint64
}

// WriteBlock returns the block at whose end the table (firstBlock, tableSize)
// is written to the index contract.
func WriteBlock(firstBlock, tableSize uint64) uint64 {
	return firstBlock + tableSize - 1 + tableSize/4
}

// DueTables returns every protocol table whose write moment is block `block`.
// The callers is responsible for filtering against fork activation
// (a table is only generated if the EIP was already active at first_block)
// and against available block history.
func DueTables(block uint64) []TableRef {
	var due []TableRef
	for _, size := range TableSizes {
		first := int64(block) - int64(size) + 1 - int64(size/4)
		if first < 0 {
			continue
		}
		fb := uint64(first)
		if fb%size != 0 {
			continue
		}
		due = append(due, TableRef{FirstBlock: fb, TableSize: size})
	}
	return due
}

// L0 returns the single-block (level 0) table for the given block.
func L0(block uint64) TableRef {
	return TableRef{FirstBlock: block, TableSize: 1}
}

// MergeGroup returns the four constituent lower-level tables that must be
// merged to build the higher-level table (firstBlock, tableSize). It is only
// meaningful for tableSize > 1.
func MergeGroup(firstBlock, tableSize uint64) []TableRef {
	sub := tableSize / 4
	group := make([]TableRef, 0, 4)
	for i := 0; i < 4; i++ {
		group = append(group, TableRef{
			FirstBlock: firstBlock + uint64(i)*sub,
			TableSize:  sub,
		})
	}
	return group
}
