# Git-Remote-AWS

Root documentation is limited to [readme.md](readme.md) for users and this file
for implementation contracts, operations, and validation. Keep the README's
quick-start path concise. Before changing an area, read its corresponding
sections below; for recipient keys/keygen, also read the
[recipient policy](readme.md#recipients-and-rotation).

## Setup and permissions

- Own concrete SDK S3/DynamoDB/STS clients from one shared-config load, with the
  five-attempt request policy and no `libaws` dependency. Preserve standard SDK v2
  profiles, credential sources, regions, and service endpoint overrides.
- `ensure=y` creates only confirmed-missing resources; discovery never converges
  existing configuration or migrates records. Anonymous HeadBucket's
  region-bearing response establishes existence without list permission.
  Other discovery errors do not imply absence. Push-time
  [versioning enablement](#versioning-and-retention) is the only existing-bucket
  configuration exception.
- New S3 buckets retain private public-access blocking, HTTPS-only policy,
  SSE-S3 encryption with SSE-C blocked, versioning enabled, no expiration, and
  the `libaws.infraset` setup tag. New DynamoDB tables use on-demand billing,
  a single string partition key `id`, TTL and streams disabled, and the same tag.
  **Never enable TTL on repository tables.**
- Serialize setup. S3's `us-east-1` CreateBucket can reset an existing owned
  bucket's ACL, so uncertain creation is not automatically retried. Every failure
  after confirmed creation, including readiness, reports incomplete setup and
  leaves the bucket in place. Inspect and complete configuration administratively
  before use; retry must not repair, delete, or recreate an existing bucket.
  Never import a general-purpose infrastructure converger.
- Setup does not create credentials or change IAM policies. Besides DynamoDB
  metadata-read and lease-write permissions, account for `s3:ListBucket` scoped
  to the namespace, `s3:GetObject` for all referenced keys (including legacy and
  shared sibling bundles), `s3:PutObject` for publication,
  `s3:AbortMultipartUpload` for owned incomplete uploads, and `s3:DeleteObject`
  for superseded manifests. Push also needs `s3:GetBucketVersioning` and, when
  enablement is needed, `s3:PutBucketVersioning`. Alias collision checks read
  DynamoDB ID `BUCKET/NAMESPACE/1`. This is not a complete provisioning IAM policy;
  setup and migration require additional administrative permissions.

## Versioning and retention

- Every helper push, including v2/no-op/empty destinations, checks versioning
  before taking a lease. Enable it if absent or suspended; an enabled bucket
  needs no configuration write. Fail closed on API errors, before uploads or
  metadata publication. This affects the **entire bucket**. List/fetch/recovery
  never check or change it. Git may omit an up-to-date helper push command,
  so that invocation has no preflight.
- After successful enablement, proceed immediately: no propagation waits or
  polling. S3 may return transient errors during propagation. Never suspend
  versioning or configure Object Lock, MFA Delete, or expiration rules.
- Delete only the superseded manifest, after confirmed publication of its complete
  replacement. Ambiguous commits preserve both candidates. With versioning,
  deletion adds a delete marker and retains the old manifest version; overwrites
  retain previous versions. No historical-manifest retention policy is added.
- Versioning is an emergency safety net, **not enforced retention**. Production
  never lists, restores, or permanently deletes object versions. Restore deleted
  or overwritten objects administratively before cloning/recovering. Keep
  permanent-version-deletion permission out of everyday writer credentials;
  privileged deletion can still destroy data.
- Never automatically delete, relocate, or garbage-collect bundles. Deleting a
  shared bundle can break every referencing manifest. Deleting one repo's
  DynamoDB record and manifests does not grant ownership of its bundles.

## Identity and layout

- URLs are `aws://BUCKET+TABLE/NAMESPACE[/REPO]`; an omitted repo is exactly `/1`,
  not the latest repo. Components start with an ASCII letter/digit and otherwise
  contain letters, digits, dots, underscores, or hyphens. Names are case sensitive.
  No numeric-suffix inference: `project3` is not shorthand for `project/3`.
- Both default aliases acquire the same lease at ID `BUCKET/NAMESPACE`; other
  repos use `BUCKET/NAMESPACE/REPO`. Reject literal legacy `/1` record collisions
  and require administrative reconciliation, never silently merge histories.
- DynamoDB is the publication authority and coordinator. Its go-dynamolock
  envelope has outer string `id` and `data: {branch, bundles}`; `bundles` is an S3
  manifest pointer. No flat-record fallback. Separate repos have separate leases
  and can publish concurrently; immutable conditional S3 creation avoids needing
  a namespace-wide lock. Pass the configured DynamoDB client explicitly.
- Force pushes, multiple remote branches, and uncommitted recipient changes remain
  forbidden. Preserve SHA-1/SHA-256 and historical encrypted bundles. Validate
  branches through cancelable native `git check-ref-format --branch`; its output
  must equal the literal input so `@{-1}` cannot expand. Slash names are valid.

Old objects stay at their original keys; new manifests reference exact locations:

```text
# Legacy default repo
NAMESPACE/0000..A
NAMESPACE/A..B
NAMESPACE/bundles_B

# New writes
NAMESPACE/.remote-aws-v2/bundles/PUSH_TIP/BASE..END
NAMESPACE/.remote-aws-v2/repos/REPO/manifests/TIP-UUID.json
```

The all-zero base has the repository's object-ID length. Bundles retain Libsodium
secretstream encryption with box recipient-key envelopes. A v2 manifest records
canonical repository, branch, tip, object format, codec, and a complete ordered
bundle chain. Each descriptor contains range, exact key, ciphertext size, ETag,
and sorted actual recipient fingerprints. Never reference another repo's manifest.
Push-tip paths prevent different pushes from replacing shared intermediate ranges;
conditional creation also protects competing same-tip writers.

Normalize legacy text lists into the common model. Reads never persist promotion.
An upgraded helper push enriches descriptors and publishes JSON without moving,
copying, or re-encrypting historical bundles. A direct no-op push can promote too;
Git may omit that command when already up to date. Siblings may adopt unpromoted
legacy lists. Follow the [upgrade procedure](#upgrading-existing-installations).

## Reuse and validation

- Empty destinations discover namespace manifests, including legacy lists and
  complete snapshots from uncertain pushes. Find ancestor endpoints of the pushed
  tip and choose a compatible complete prefix minimizing the remaining commit
  count. Adopt stored boundaries rather than recreating them. Existing
  destinations retain their exact chains and require fast-forward updates.
- Validate version, codec, object format, literal branch, repository scope, safe
  keys, and a continuous chain from the zero base to declared tip: no gaps,
  cycles, or mixed formats. Validate native Git ancestry, object existence,
  sizes, and ETags. Read actual ciphertext recipient fingerprints with bounded,
  ETag-conditional envelope range requests; never infer historical policy from
  `.publickeys` at an intermediate commit.
- Cross-repo reuse requires one ciphertext recipient per current key chain and
  no extras. Retained generations are compatible; added/removed recipients or
  replaced identities may require repacking. Existing destinations retain their
  historical key requirements. New bundles use the final pushed tip's policy.
- These checks do not audit the full payload. Fetch pins reads to recorded ETags,
  checks length, authenticates ciphertext, verifies bundle heads/prerequisites,
  and checks the selected tip's reachable object closure. Do not run whole-ref
  fsck inside the helper: clone temporarily uses `.invalid` HEAD. A fresh clone
  followed by `git fsck --strict` provides an end-to-end check. S3 write access is
  a trust boundary; unsigned manifests and recipient routing fingerprints do not
  prove authenticity against a malicious storage administrator.
- During adoption, skip only typed missing-manifest responses for disappearing
  superseded snapshots. Missing/changed bundles, malformed manifests, and other
  failures remain errors. Lost DynamoDB metadata never authorizes rewriting
  surviving S3 history. Complete snapshots from unacknowledged initial pushes may
  be retried at the same tip or extended; conflicting snapshots require reconciliation.
- Ordinary list/fetch read the DynamoDB pointer and manifest together, with at
  most three attempts. Only typed S3 `NoSuchKey` for that manifest permits
  rediscovery; changed branch, lost pointer, other failures, or exhaustion remain
  errors. Missing bundles never trigger it. Push keeps its lease-protected read.

### Validation cache

Deduplicate inspection of distinct objects within each adoption/promotion
operation; conflicting descriptors stay fatal. Persist successful envelope checks,
including post-upload validation, under Git-resolved
`git-remote-aws/validation-v2/` metadata, including linked worktrees. Every new
operation still HEADs each distinct object. Cache hits require unchanged resolved
endpoint, bucket, key, range, codec, ETag, and size. Known recipient policies permit
one bounded envelope GET; still verify the stored count and fingerprints.

Missing, corrupt, or inaccessible local entries are misses; remote corruption or
identity changes remain errors. Writes are atomic and best-effort. Deleting the
cache safely forces fresh header reads. Never replace ancestry checks or fetch
authentication with a cache hit. Like other Git metadata, the cache is trusted
local state, not a security boundary against someone who can rewrite it.

## Publication and cancellation

- Protected requests, push Git work, and encryption use the lease context.
  Commit metadata only after every bundle and the complete manifest upload
  succeed. Commit cancels the lease context. Deferred cleanup uses bounded,
  independent `Release`, never writes a possibly modified payload, and preserves
  the original push error, naming a lost lease's cause before release. Ambiguous
  commits must not cause another payload write or deletion of the previous
  manifest.
- One SIGINT/SIGTERM context spans initialization and the helper protocol loop,
  including idle input waits. Pass it through list/fetch/push; the lease context
  remains authoritative for protected work. Post-commit S3 cleanup uses the helper
  context, not the canceled lease context.
- Use `gitCommand`, not bare `exec.CommandContext`: it isolates process groups,
  kills descendants on cancellation, and bounds inherited-pipe waits. Encryption
  and decryption retain shared codecs, check context between chunks, and close
  owned streams to interrupt blocked I/O.

## Bundle sizing and uploads

- The default soft target is 256 MiB compressed, before encryption. Configure
  `remote-aws.bundleSize` with a positive byte count; Git's binary `k`, `m`, and
  `g` suffixes are supported. Git must support `rev-list --disk-usage`.
- Estimate before packing and check actual sizes. Keep boundaries on an ancestry
  chain, including when the remote base is reached through a merge's non-first
  parent. Merges bring all required side history. An indivisible first-parent
  increment may exceed the target; this is not a hard maximum or exact bin packing.
  Retain only one temporary plaintext/ciphertext pair at a time.
- Historical larger bundles remain indivisible for adoption; rewrites within one
  may require repacking its unshared suffix. Packing boundaries vary with tip,
  Git version, local packing, and size settings. Reuse does not depend on
  deterministic packing or encryption.
- Temporary boundary refs under `refs/git-remote-aws/` are unique and conditionally
  pinned. Only confirmed creation grants cleanup ownership. Cleanup is bounded
  and independent and must preserve concurrent changes. Prepare conditional
  deletion and check the ref is still direct while holding Git's transaction
  lock, then commit: an old-OID guard with `--no-deref` alone accepts symbolic
  replacements. Interrupted creation or failed cleanup reports the ref for
  administrative inspection before removal.
- Ciphertext larger than 64 MiB uses sequential multipart uploads, normally with
  64 MiB parts that grow for exceptionally large files to respect S3's part limit.
  Retries replay only affected parts. Multipart is transport only: preserve
  range names, shared codecs, and DynamoDB layout.
- Exact-tip retries list only that push's bundle prefix, then retain the longest
  contiguous completed prefix, preferring fewer bundles and stable key ordering.
  Completed boundaries remain immutable when size settings or packing change;
  only new ranges use the new target. Listing is not validation: require fresh
  size/ETag and push-tip checks plus actual recipients before reuse. Validate with
  at most eight workers; cancel and join all on failure before publication.
  Pushes of other tips cannot reuse incomplete leftovers. Never overwrite or
  delete conflicting or unselected leftovers.
- Abort only the owned multipart ID with a bounded independent context; preserve
  primary errors and completed objects on ambiguous completion. Abrupt termination
  or uncertain initiation can leave incomplete uploads for administrative cleanup.
  Never configure lifecycle rules to clean them up automatically.

## Upgrading existing installations

There are two independent format changes:

1. **Manifest promotion:** upgrade all writers before publishing v2 JSON manifests;
   old clients cannot read them. Let old-helper uploads finish first. Promotion
   happens on a subsequent helper push without rewriting historical ciphertext.
   No separate S3 migration is required.
2. **Lease-envelope migration:** tables predating the `data` envelope require the
   procedure below. Namespace promotion does not bypass it.

### Migrating older tables

Stop **all** old and new helpers, including automated pushes, before migration;
upgrade every client before resuming. Old helpers cannot safely access migrated
tables. The string `id` partition key and S3 objects do not change; `branch` and
`bundles` move under `data`. Do not enable TTL.

Build without replacing the installed helper, then preview one bucket's records:

```sh
go build -o /private/path/git-remote-aws .
/private/path/git-remote-aws --migrate-dynamolock \
  --account ACCOUNT_ID --region REGION --table TABLE --bucket BUCKET
```

Repeat with `--apply --writers-stopped --backup /private/path/metadata.json` to
migrate. The backup must be a new file, saved with mode `0600` before any writes.
Retain it until upgraded clients have been verified.

`migration.go` is read-only by default, never a helper fallback. Preserve
account/table/bucket scoping, schema/TTL validation, original-JSON backup,
unchanged-record conditional writes, and final snapshot verification. Refuse
uncleared locks, mixed schemas, and unknown fields; leave migrated records alone.
`--id BUCKET/REPOSITORY` limits the operation to strongly consistent keyed reads,
never scans. Migration does not modify S3.

## S3-only emergency recovery

Recover only from an explicitly selected self-contained manifest into a new,
nonexistent directory. Recovery uses only S3 reads and the normal private-key
loader, imports through the common fetch path, and checks out the selected tip.
Never write AWS metadata, choose the latest candidate automatically, or infer
which push was acknowledged. A failed recovery may leave its new directory for
inspection. Pre-promotion text lists lack branch metadata and cannot serve as
self-contained recovery manifests.

```sh
# List candidate keys, branches, and tips without DynamoDB access.
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO

# Explicitly select one candidate.
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO \
  --manifest NAMESPACE/.remote-aws-v2/repos/REPO/manifests/TIP-UUID.json \
  --directory /path/to/new-checkout
```

Recovery sees visible objects only, not historical S3 versions. Restore deleted
or overwritten objects administratively first. Rebuilding the DynamoDB pointer
is a separate administrative action; stop writers before doing so.

## Validation

### Cloud-free checks

- Prerequisites: [build dependencies](readme.md#install), Bash, Python 3.8+ for
  CLI/terminal tests, and `libcheck` on PATH
  (`go install github.com/nathants/libcheck@latest`) with its pinned tools
  installed. libcheck verifies tool versions and never installs anything; its
  `security` step needs network.
- Gate: `GOTOOLCHAIN=local bash bin/check.sh`. Runs libcheck lint and `security`
  (govulncheck), builds, and the full cloud-free selection with race
  instrumentation. Lint policy belongs to libcheck: keep no linter configuration
  here, and follow its readme for suppressions and `libcheck.json` exceptions.
- Cloud-free selection: `GOTOOLCHAIN=local go test -race -count=1 -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...`.
- Migration units alone: `go test -count=1 -run '^TestMigration' ./...`.
- Test helpers build into `t.TempDir()`, never over the installed helper,
  especially during incompatible table cutovers. Lease error tests execute
  `main`, including CLI recovery, so cleanup cannot silently replace the primary error.
- OS-signal tests must not SIGSTOP child groups: Linux sends SIGHUP to stopped
  orphaned groups, masking missing helper cancellation. Lease-context cancellation
  tests that keep the helper alive are different.

### Live AWS gate

- Run `GOTOOLCHAIN=local GOFLAGS=-race go test -count=1 -timeout=60m ./...` with
  credentials and a region for an independently known scratch account, and
  `GIT_REMOTE_AWS_TEST_ACCOUNT` set to that account; live tests skip without it.
  Tests verify the account through STS before any helper runs with `ensure=y`.
  Never use production credentials.
- Each live test creates a fresh `git-remote-aws-test-UUID` bucket and table
  through the helper's `ensure=y` setup; `TestEnsureSetupAWS` verifies the
  configuration AWS applied. Test cleanup, which also runs after panics,
  permanently deletes every object version, delete marker, and incomplete upload,
  then the bucket and table. Beyond setup permissions, it needs
  `s3:ListBucketVersions`, `s3:DeleteObjectVersion`,
  `s3:ListBucketMultipartUploads`, `s3:DeleteBucket`, and `dynamodb:DeleteTable`.
  A killed or timed-out run skips cleanup; remove leftover
  `git-remote-aws-test-*` resources administratively.
- Keep `TestNamespace*AWS` in the full live gate: these cover legacy promotion/reuse
  and actual concurrent writers to separate and shared destination leases.
- `TestDynamolockMigration` runs the migration CLI and then reads, acquires, and
  commits with the library against AWS. Its stale-preimage test submits writes
  after payload/ownership changes, deletion, and competing migration, verifying
  that DynamoDB rejects them without changing the winner.

### Historical compatibility

- Old data must come from old code: tests build helpers on demand from the
  revisions pinned in `historical_test.go`, each with its own module graph.
  Never patch those trees, update their dependencies, substitute newer code, or
  check in generated fixtures; if a pinned revision stops building, discuss it.
  Offline generation must reach only the scripted provider.
- Keep covering legacy histories in both object formats that are read, promoted,
  extended, and reused without rewriting ciphertext, and live pre-keychain full
  and incremental pushes with an untracked recipient file through metadata
  migration, clone, rotation, incomplete-key rejection, and unchanged old ETags.
- Cloud-free namespace tests also cover recipient rotation/replacement,
  corruption/gaps, pagination, alias collisions, lost pointers, and S3-only recovery.
