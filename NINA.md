# Git-Remote-AWS

Read [readme.md](readme.md) for the remote URL, recipient-key policy, and the
lease-format table cutover before changing storage or push behavior.

## Storage invariants

- Repository metadata uses go-dynamolock's envelope: outer string `id` is
  `BUCKET/PREFIX`; `branch` and `bundles` live under `data`. No flat-record
  fallback. The table has only a string partition key `id`, with TTL disabled.
- Pass the configured DynamoDB client explicitly. Use the lease context for
  protected requests and bundle creation. Commit metadata only after both S3
  uploads succeed. Deferred cleanup must use bounded, independent `Release`,
  never write the possibly modified payload. Commit cancels the lease context;
  post-commit S3 cleanup cannot reuse that context. Ambiguous commits must not
  trigger another payload write or deletion of the previous manifest.
- All clients must stop for a lease-format migration and upgrade before resuming.
  `migration.go` implements the read-only-by-default `--migrate-dynamolock`
  command, not a helper fallback. It scopes by account/table/bucket, backs up original
  DynamoDB JSON before writes, conditionally transforms unchanged records, and
  verifies the final snapshot. It does not modify S3.
- Force pushes, multiple remote branches, and uncommitted recipient changes
  remain forbidden. Preserve SHA-1/SHA-256 and historical encrypted bundles.

## Validation

- Gate: `GOTOOLCHAIN=local bash bin/check.sh`. Its Go analysis tools
  must already be installed; the existing script installs missing Go tools.
  Check prerequisites first rather than allowing an unapproved installation.
- Cloud-free tests:
  `GOTOOLCHAIN=local go test -race -count=1 -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...`.
  Migration unit tests are included in the gate and can run separately with
  `go test -count=1 -run '^TestMigration' ./...`.
- Live gate: `GOTOOLCHAIN=local GOFLAGS=-race go test -count=1 -timeout=15m ./...`
  with `GIT_REMOTE_AWS_TEST_ACCOUNT`, `GIT_REMOTE_AWS_TEST_BUCKET`, and
  `GIT_REMOTE_AWS_TEST_TABLE`. Use an independently known scratch account and
  disposable, pre-provisioned unversioned bucket/id-keyed table. Never point tests
  at production or go-dynamolock's reusable test table. Remove scratch resources
  after confirming test cleanup.
- Test helpers build into `t.TempDir()`, never over the installed helper. This
  is essential during incompatible table cutovers.
- `TestDynamolockMigration` runs the actual migration CLI and then reads, acquires,
  and commits with the new library against AWS. The historical compatibility
  test migrates metadata between old-helper writes and new-helper reads; it also
  checks old encrypted bundle ETags after cloning and rotation. Supply the
  independently pinned old artifact described in the README to run it.
