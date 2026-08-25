# Merge fixtures (Week 10)

The `merge/v1` corpus exercises the four-way merge that produces higher-level
EIP-8304 index tables. Each fixture contains exactly four blocks and exposes
two stages of the calculation chain:

1. `per_block_l0` — each block's sorted level-0 table (recomputed from the
   production builder);
2. `merged_sorted`, `merged_leaf_hashes`, `merged_entry_count` — the result of
   merging those four tables.

The merged result must equal re-sorting the concatenation of the four tables
(a four-way merge is a full ordered union). Block entries follow the
one-block-delay rule: a table covering blocks `[40..43]` contains block entries
for `39..42`.

`merged_entry_count` is the SSZ list length that will be mixed into the table
root once the SSZ list bound is confirmed. No `table_root` is published here
while the SSZ bound remains `<TBD>`. The running implementation is
`execution/protocol/eip8304/merge.go`; the runner is
`execution/protocol/eip8304/merge_fixtures_test.go`.

```bash
cd erigon
go test ./execution/protocol/eip8304
```

Leaf hashes were independently recomputed with the system `sha256sum` from the
sorted encoded entries.
