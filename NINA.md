# Git-Remote-AWS

Read [readme.md](readme.md) for the remote URL, recipient-key policy, and the
lease-format table cutover before changing storage or push behavior. Read
[shared namespace storage](storage.md) before changing manifests, adoption, URL
identity, compatibility, or recovery.

## Storage invariants

- The helper owns concrete AWS SDK S3/DynamoDB/STS clients from one shared-config
  load, retaining the previous five-attempt request policy. It has no `libaws`
  dependency. `ensure=y` creates only confirmed-missing resources; existing
  resource configuration and records remain unchanged during discovery, with no
  data migration. Push versioning preflight is the explicit exception below.
  Anonymous HeadBucket's region-bearing response still establishes existence
  without requiring list permission. Other discovery errors do not imply absence.
- Read the setup details in [readme.md](readme.md#usage) before changing `aws.go`.
  Preserve new-resource security defaults and the existing `libaws.infraset` tag;
  never copy a general-purpose infrastructure converger into the helper. Setup
  must be serialized and uncertain CreateBucket is not automatically retried.
  Every failure after confirmed bucket creation, including the readiness wait,
  must report incomplete setup requiring administrative completion rather than
  deletion or general configuration convergence of a now-existing bucket.
- New buckets enable versioning. Every helper push (including v2/no-op/empty
  destinations) checks versioning before taking a lease, enables it if absent or
  suspended, and fails closed on API errors. Successful enablement proceeds
  immediately, without propagation waits or post-enablement polling. No
  Object Lock, MFA Delete, expiration, version-aware recovery or production
  version deletion. List/fetch/recovery do not check or change versioning.

- Repository metadata uses go-dynamolock's envelope: outer string `id` is
  `BUCKET/PREFIX`; `branch` and `bundles` live under `data`. No flat-record
  fallback. The table has only a string partition key `id`, with TTL disabled.
- Pass the configured DynamoDB client explicitly. Use the lease context for
  protected requests, push Git work, and encryption. Commit metadata only after
  every encrypted bundle and the cumulative list upload succeed. Deferred cleanup
  must use bounded, independent `Release`, never write the possibly modified
  payload. Commit cancels the lease context.
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
- Read [bundle sizing](readme.md#bundle-sizing-and-large-pushes) before changing
  `bundle_split.go` or `bundle_upload.go`. Initial and incremental pushes use a
  256 MiB soft compressed-bundle target (`remote-aws.bundleSize`). Estimate before
  packing and check actual sizes; keep boundaries on an ancestry chain, including
  when the remote base is reached through a merge's non-first parent. An
  indivisible first-parent increment can exceed the target. Only one bundle's
  temporary plaintext/ciphertext pair is retained at a time. Boundary refs are
  unique and pinned with conditional Git updates; their bounded, independent
  cleanup must not move or delete a concurrently changed ref. Only confirmed
  creation grants cleanup ownership. Prepare the conditional deletion and check
  that the ref is still direct while holding Git's transaction lock, then commit;
  an old-OID guard with `--no-deref` alone also accepts symbolic replacements.
  Interrupted creation can leave a ref whose name is reported for inspection.
- Namespace URLs are `NAMESPACE[/REPO]`, with `/1` implicit. Both default aliases
  retain DynamoDB ID `BUCKET/NAMESPACE`; other IDs are `BUCKET/NAMESPACE/REPO`.
  Reject collisions with literal legacy `/1` records. No numeric-suffix inference.
- `manifest.go` normalizes legacy text lists into the common model. Read operations
  never persist promotion. Push writes self-contained version-2 manifests with
  exact keys, continuous ranges, ETags/sizes, codec/format, branch/tip and actual
  recipient fingerprints; it never moves or rewrites historical ciphertext.
  Upgrade all writers first. Empty destinations adopt compatible existing prefixes;
  existing destinations retain their own chains. Never turn missing DynamoDB
  metadata into permission to rewrite a surviving S3 history.
- Adoption/promotion deduplicates object inspection within an operation and
  persists successful envelope checks under Git-resolved metadata
  `git-remote-aws/validation-v2/`. Every new operation still HEADs distinct objects;
  cache hits require unchanged endpoint/bucket/key/range/codec/ETag/size. Local
  corrupt/missing cache is a miss; remote corruption/conflicting descriptors
  stays fatal. Cache does not replace native ancestry or fetch authentication.
- Adopted chains must cover the root through the reuse endpoint. Validate native
  Git ancestry, scope, object identities and actual envelope recipients (bounded,
  ETag-conditional range reads). A retained key generation is compatible; unknown
  or different recipient identities are not. Fetch authenticates ciphertext and
  verifies heads, prerequisites and reachable object closure. Do not run a whole
  ref-database fsck inside the helper: Git clone temporarily uses `.invalid` HEAD.
- Multipart upload is transport only: preserve range names, shared codecs, and
  DynamoDB layout. Newly created bundles use the final push tip's recipient policy.
  Conditional S3 creation and push-tip paths/metadata prevent stale writers from
  overwriting another push's ciphertext. Exact-tip retry preflight avoids another
  transfer of completed objects. Never backfill/delete conflicting leftovers.
  Abort only the owned multipart upload ID with a bounded independent context;
  preserve primary errors and completed objects on ambiguous completion.
- `--recover` lists/selects self-contained manifests and imports into a new
  directory through the common fetch path using only S3 reads. It does not write
  DynamoDB, infer the last acknowledged push, or automatically choose a snapshot.
- Push/fetch branch refs use native `git check-ref-format --branch` through the
  cancelable Git runner. Valid slash names are supported; the returned name must
  equal the literal input so checkout expressions such as `@{-1}` cannot expand.
- Read-only list/fetch discovery reads the DynamoDB pointer and its bundle list
  together, with at most three discovery attempts. Only the SDK's typed S3
  `NoSuchKey` for that list triggers rediscovery; a changed branch, lost pointer,
  other failure, or exhausted budget remains an error. Missing encrypted bundles
  do not trigger it. Push keeps its lease-protected read, and successful pushes
  still delete the old cumulative manifest; no historical-manifest retention or
  bundle garbage collection is added.

## Validation

- Gate: `GOTOOLCHAIN=local bash bin/check.sh`. All nine Go analysis tools listed
  in its prerequisite loop must already be on PATH; the script fails before
  checks when any is missing and never installs tools. The gate also runs the
  full cloud-free selection below with race instrumentation.
- Cloud-free tests:
  `GOTOOLCHAIN=local go test -race -count=1 -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...`.
  Migration unit tests can run separately with
  `go test -count=1 -run '^TestMigration' ./...`.
- Live gate: `GOTOOLCHAIN=local GOFLAGS=-race go test -count=1 -timeout=15m ./...`
  with `GIT_REMOTE_AWS_TEST_ACCOUNT`, `GIT_REMOTE_AWS_TEST_BUCKET`, and
  `GIT_REMOTE_AWS_TEST_TABLE`. Use an independently known scratch account and
  disposable, pre-provisioned versioned bucket/id-keyed table. Test cleanup alone
  permanently deletes owned namespace versions/delete markers. Never point tests
  at production or go-dynamolock's reusable test table. Remove scratch resources
  after confirming test cleanup.
- Namespace cloud-free coverage uses small real Git bundles, both object formats,
  legacy ciphertext fixtures, recipient rotation/replacement, corruption/gaps,
  pagination, alias collisions, lost pointers, and S3-only recovery. Live
  `TestNamespace*AWS` covers legacy promotion/reuse and actual concurrent writes
  to separate/same destination leases. Keep these in the full live gate.
- Read [fixture provenance and regeneration](testdata/README.md) before
  regenerating stored-data compatibility fixtures; use the retained pre-removal
  producer, never current production code.
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

