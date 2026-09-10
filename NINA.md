# Git-Remote-AWS

Read [readme.md](readme.md) for the remote URL, recipient-key policy, and the
lease-format table cutover before changing storage or push behavior.

## Storage invariants

- The helper owns concrete AWS SDK S3/DynamoDB/STS clients from one shared-config
  load, retaining the previous five-attempt request policy. It has no `libaws`
  dependency. `ensure=y` creates only confirmed-missing resources; existing
  resource configuration and records remain unchanged, with no data migration.
  Anonymous HeadBucket's region-bearing response still establishes existence
  without requiring list permission. Other discovery errors do not imply absence.
- Read the setup details in [readme.md](readme.md#usage) before changing `aws.go`.
  Preserve new-resource security defaults and the existing `libaws.infraset` tag;
  never copy a general-purpose infrastructure converger into the helper. Setup
  must be serialized, uncertain CreateBucket is not automatically retried, and
  interrupted new-bucket configuration needs administrative completion rather
  than deletion or automatic mutation of a now-existing bucket.

- Repository metadata uses go-dynamolock's envelope: outer string `id` is
  `BUCKET/PREFIX`; `branch` and `bundles` live under `data`. No flat-record
  fallback. The table has only a string partition key `id`, with TTL disabled.
- Pass the configured DynamoDB client explicitly. Use the lease context for
  protected requests, all push Git commands, and encryption. Commit metadata only
  after both S3 uploads succeed. Deferred cleanup must use bounded, independent `Release`,
  never write the possibly modified payload. Commit cancels the lease context.
  Ambiguous commits must not trigger another payload write or deletion of the
  previous manifest. A release failure must preserve the original push error,
  including commit ambiguity.
- The helper owns one SIGINT/SIGTERM context across initialization and the
  protocol loop, including idle input waits. Pass it through list/fetch/push;
  push's lease context remains the authority for protected work. Post-commit S3
  cleanup uses the helper context, not the canceled lease context.
- `gitCommand` puts Git in its own process group, kills descendants on
  cancellation, and bounds inherited-pipe waits. Do not replace it with bare
  `exec.CommandContext`. Bundle encryption and decryption keep the shared codecs
  but check context between chunks and close owned streams to interrupt blocked I/O.
- All clients must stop for a lease-format migration and upgrade before resuming.
  `migration.go` implements the read-only-by-default `--migrate-dynamolock`
  command, not a helper fallback. It scopes by account/table/bucket, backs up original
  DynamoDB JSON before writes, conditionally transforms unchanged records, and
  verifies the final snapshot. Exact-ID snapshots use strongly consistent keyed
  reads, never scans. It does not modify S3.
- Force pushes, multiple remote branches, and uncommitted recipient changes
  remain forbidden. Preserve SHA-1/SHA-256 and historical encrypted bundles.
- Push/fetch branch refs use native `git check-ref-format --branch` through the
  cancelable Git runner. Valid slash names are supported; the returned name must
  equal the literal input so checkout expressions such as `@{-1}` cannot expand.
- Read-only list/fetch discovery reads the DynamoDB pointer and its bundle list
  together, with at most three discovery attempts. Only the SDK's typed S3
  `NoSuchKey` for that list triggers rediscovery; a changed branch, lost pointer,
  other failure, or exhausted budget remains an error. Missing encrypted bundles
  do not trigger it. Push keeps its lease-protected read, and successful pushes
  still delete the old cumulative list; no historical-list retention is added.

## Validation

- Gate: `GOTOOLCHAIN=local bash bin/check.sh`. Its Go analysis tools
  must already be installed; the existing script installs missing Go tools.
  Check prerequisites first rather than allowing an unapproved installation. The
  gate also runs the full cloud-free selection below with race instrumentation.
- Cloud-free tests:
  `GOTOOLCHAIN=local go test -race -count=1 -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...`.
  Migration unit tests can run separately with
  `go test -count=1 -run '^TestMigration' ./...`.
- Live gate: `GOTOOLCHAIN=local GOFLAGS=-race go test -count=1 -timeout=15m ./...`
  with `GIT_REMOTE_AWS_TEST_ACCOUNT`, `GIT_REMOTE_AWS_TEST_BUCKET`, and
  `GIT_REMOTE_AWS_TEST_TABLE`. Use an independently known scratch account and
  disposable, pre-provisioned unversioned bucket/id-keyed table. Never point tests
  at production or go-dynamolock's reusable test table. Remove scratch resources
  after confirming test cleanup.
- Test helpers build into `t.TempDir()`, never over the installed helper. This
  is essential during incompatible table cutovers. Lease error tests execute
  `main`, including CLI recovery, so cleanup cannot silently replace the primary error.
- OS-signal tests must not stop child process groups with SIGSTOP: Linux sends
  SIGHUP to stopped orphaned groups, which can mask missing helper cancellation.
  Lease-context cancellation tests that keep the helper alive are different.
- `TestDynamolockMigration` runs the actual migration CLI and then reads, acquires,
  and commits with the new library against AWS. The stale-preimage test submits
  actual migration writes after changed payload/ownership, deletion, and competing
  migration, and verifies DynamoDB rejects them without changing the winner.
  The historical compatibility test migrates metadata between old-helper writes
  and new-helper reads; it also
  checks old encrypted bundle ETags after cloning and rotation. Supply the
  independently pinned old artifact described in the README to run it.
