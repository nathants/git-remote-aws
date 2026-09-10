# Pre-libaws-removal compatibility fixtures

`libaws-sha1.json` and `libaws-sha256.json` contain actual encrypted Git bundles,
their cumulative bundle lists and their DynamoDB `data` payloads, published by
helper revision `3ebf0a3` with its pinned libaws dependency. The full producer
commit is recorded in each fixture. All keys and content are synthetic; the
included private keys are public test fixtures, never production recipients.

`TestBundleLibawsRemovalCompatibility` lists, decrypts/imports and extends these
histories with the current helper, without migrating records or rewriting the
historical ciphertext. Provider responses are scripted, not a simulation of
DynamoDB's condition evaluator; the separate lease tests cover that protocol.

The fixtures were generated before changing production code, using the test-only
`GIT_REMOTE_AWS_GENERATE_LIBAWS_FIXTURES=1` path. Normal tests never regenerate
them. Regeneration must use the recorded producer's production sources and
module graph, with only the compatibility test/generator files overlaid. Do not
replace them with fixtures produced by the current helper.
