# Pre-libaws-removal compatibility fixtures

`libaws-sha1.json` and `libaws-sha256.json` contain actual encrypted Git bundles,
their cumulative bundle lists and their DynamoDB `data` payloads, published by
helper revision `3ebf0a3` with its pinned libaws dependency. The full producer
commit is recorded in each fixture. All keys and content are synthetic; the
included private keys are public test fixtures, never production recipients.

`TestBundleLibawsRemovalCompatibility` lists, decrypts/imports and extends these
histories with the current helper, promoting only manifests without rewriting
historical ciphertext. `TestBundleLegacyPromotionWithoutUpload` also verifies
metadata-only promotion and cross-repo reuse, and `TestNamespaceLegacyAdoptionAWS`
exercises these retained ciphertexts against actual S3 and DynamoDB. Cloud-free
provider responses are scripted, not a simulation of DynamoDB's condition
evaluator; the separate lease tests cover that protocol.

The fixtures were generated before changing production code, using the test-only
`GIT_REMOTE_AWS_GENERATE_LIBAWS_FIXTURES=1` path. Normal tests never regenerate
them. For regeneration, use retained revision
`672701aae88116fe04e9e391cf942db1c6066024`. Its tree
`b04c68484a051424749c19a14f6183d9d68c26ba` is identical to the original producer's,
including all production sources and the module graph. The original producer is
no longer retained by a branch or tag; existing fixtures keep their original
`producer` values and ciphertext.

In a detached worktree at the retained revision, overlay the fixture type,
`TestBundleLibawsRemovalCompatibility`, and `generateLibawsCompatibilityFixture`
from `libaws_compatibility_test.go`. Do not include the later bundle-sizing test
or change production files or dependencies. Run
`GIT_REMOTE_AWS_GENERATE_LIBAWS_FIXTURES=1 go test -count=1 -run '^TestBundleLibawsRemovalCompatibility$'`.
Generated fixtures record the retained revision as their producer. Never replace
historical fixtures with ones produced by the current helper.
