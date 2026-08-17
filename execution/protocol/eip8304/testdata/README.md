# EIP-8304 fixtures

The `v1` vectors target EIP revision `81b976ac01591fed2eecb73fa574f27cd18db2e8` and the schema in `schema-v1.json`.

The `schema-negative` corpus contains malformed quantities, hashes, and addresses. Every JSON file there must fail `schema-v1.json`; parser unit tests separately verify their diagnostic codes. Only schema-valid protocol errors belong in the `v1` `expected_error` enum.

To update a vector:

1. Change the smallest input that demonstrates one protocol rule.
2. Calculate chronological entries directly from the pinned EIP encoding.
3. Sort the complete encoded byte strings and calculate SHA2-256 over each sorted entry.
4. Update the golden output and run `go test ./execution/protocol/eip8304`.
5. Validate every JSON file against `schema-v1.json` and independently recalculate the leaf hashes.
6. Mirror the reviewed vectors to the project-level fixture directory used for cross-client sharing.

Do not add `table_root` while the SSZ list limit and fixed tree depth remain unresolved. A change to the fixture schema or pinned EIP revision requires a new version rather than rewriting the meaning of `v1`.
