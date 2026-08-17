# Schema-negative fixtures

Every JSON file in this directory must fail validation against `../schema-v1.json` for the malformed field named by the file. They are separate from `v1`: schema-valid negative vectors test protocol errors, while this corpus tests malformed JSON representations.

The Go parser tests cover the corresponding stable diagnostic codes. Do not add these codes to the schema-valid `expected_error` enum.
