# Git-Remote-AWS

Encrypted Git hosting in S3, with DynamoDB compare-and-swap for concurrent pushes.
Each remote has one branch; force pushes are forbidden. Repos within a namespace
share immutable encrypted bundles without sharing a branch tip. Valid branch names,
including `archive/home` are supported. SHA-1 and SHA-256 Git repositories and
existing encrypted histories remain readable.

Git bundles use Libsodium secretstream encryption and box recipient keys. Branch
names, remote names, and bundle boundary commit IDs are unencrypted. Encryption
keys and AWS access credentials are separate.

## Install

Requires Linux, Go 1.27+, and Libsodium (including development headers).

```sh
go install github.com/nathants/git-remote-aws@latest
export PATH="$PATH:$(go env GOPATH)/bin"
```

## Keys

Create personal key files in a private directory:

```sh
mkdir -p -m 700 ~/.config/git-remote-aws
git-remote-aws --keygen \
  --public-key-file ~/.config/git-remote-aws/public \
  --secret-key-file ~/.config/git-remote-aws/secret
export GIT_REMOTE_AWS_SECRETKEY_FILE=~/.config/git-remote-aws/secret
```

Set exactly one nonempty secret source:

- `GIT_REMOTE_AWS_SECRETKEY`: private key chain text.
- `GIT_REMOTE_AWS_SECRETKEY_FILE`: private file, mode `0600` or `0400`.
- `GIT_REMOTE_AWS_SECRETKEY_CMD`: executable that prints private chains; receives
  the remote URL as its argument. Interactive loaders run in the foreground;
  Ctrl-C cancels loading. No prompting deadline is imposed.

Secrets load only for decryption. Without file flags, `--keygen` prints a fresh
pair as export statements for manual secret-manager storage; keep that output private.

## Usage

Configure AWS credentials, then create a repository:

```sh
mkdir myrepo && cd myrepo
git init -b main
cat ~/.config/git-remote-aws/public > .publickeys
git add .publickeys
git commit -m 'Add recipients'
git remote add origin aws://BUCKET+TABLE/REPOSITORY
git push -u origin main
```

Use `ensure=y git push ...` to create a missing private bucket/table with suitable
AWS permissions. Otherwise provision them beforehand. Setup uses the AWS SDK
directly, not the `libaws` library or CLI. Only confirmed absence permits creation;
permission, transport and other discovery failures remain errors. Existing bucket
configuration, table schema/billing/TTL, S3 objects and DynamoDB records are not
converged or migrated. This dependency change requires no data-at-rest migration.

New buckets retain the private public-access block, HTTPS-only policy, default
SSE-S3 encryption with SSE-C blocked, no versioning or expiration, and the existing
`libaws.infraset` setup tag. New tables retain on-demand billing and the single
string partition key `id`, with TTL and streams disabled. Setup does not create
credentials or change IAM permissions. A failure after bucket creation leaves the
bucket in place and reports incomplete setup; inspect and finish its configuration
administratively before use. Retry never deletes/recreates resources or converges
an already existing bucket. Serialize resource setup: S3's `us-east-1` create API
can reset an existing owned bucket's ACL, so the helper does not automatically
retry an uncertain `CreateBucket` response.

AWS shared profiles, credential sources, regions and service endpoint overrides
use standard SDK v2 resolution. Clients share one loaded configuration per helper
invocation, retaining the five-attempt request retry policy except for bucket
creation. Clone and fetch work normally.

URLs are `aws://BUCKET+TABLE/NAMESPACE[/REPO]`. An omitted repo is exactly `/1`;
`WickedEngine3` and `WickedEngine3/1` are aliases, not a mapping to
`WickedEngine/3`. To start a rewritten history, choose another repo in the same
namespace, such as `WickedEngine3/2`. Its first push automatically adopts existing
compatible ancestry chunks and uploads only the remaining suffix. Existing repos
still forbid rewrites. See [shared namespace storage](storage.md) for identity,
layout, permissions, validation, promotion, and recovery.

If a concurrent push deletes the bundle list a reader just discovered, list/fetch
rediscover the current pointer and list, up to three total attempts. Other errors
and persistent absence still fail. Old lists are not retained indefinitely.

`.publickeys` contains one recipient per line, with rotations joined by colons:
`old:new:newest`. New bundles use each recipient's newest key from the pushed
commit. Commit recipient edits before pushing; staged or unstaged edits are rejected.
Rows need not be sorted; blank lines and a missing final newline are allowed.

## Bundle sizing and large pushes

Initial and incremental pushes target **256 MiB per compressed Git bundle**, before
stream encryption. Override the soft target with a positive byte count; Git's
binary `k`, `m`, and `g` suffixes are supported:

```sh
git config remote-aws.bundleSize 256m
```

The helper estimates object storage before packing, splits large commit ranges,
and checks actual bundle sizes afterward. Boundaries follow one first-parent
ancestry chain; a merge brings along all required side history. A single such
increment can exceed the target, so this is not a hard maximum or exact bin
packing. Bundles are created, encrypted, uploaded, and removed from temporary
storage one at a time. Allow temporary disk space for both the plaintext and
ciphertext of the largest increment. Git must support `rev-list --disk-usage`.

Temporary boundary refs live under `refs/git-remote-aws/`. Cleanup preserves
concurrent changes. Interrupted creation or failed cleanup can leave a ref; errors
report its name for administrative inspection before removal.

Encrypted files larger than 64 MiB use sequential S3 multipart uploads, normally
with 64 MiB parts. Part sizes grow for exceptionally large files to stay within
S3's part-count limit. Retries replay only the affected part, not the entire
file. Multipart uses the existing `s3:PutObject` permission and additionally
needs `s3:AbortMultipartUpload` for cleanup. Cancellation and upload failures
attempt a bounded abort without hiding the original failure. Abrupt process
termination or an uncertain initiation response can leave incomplete uploads;
inspect/abort those administratively. The helper does not change bucket lifecycle
rules or IAM policies.

All newly created bundles in a push use the final pushed commit's recipient
policy. Inherited bundles retain their actual historical policy; cross-repo
adoption checks the ciphertext envelope against the current recipient key chains.
The helper publishes one self-contained JSON manifest and commits its DynamoDB
pointer only after every dependency is durable. The previous cumulative manifest
is deleted only after a confirmed metadata commit. Bundles are never deleted,
moved, or automatically garbage-collected.

**Upgrade all writers before using the new manifest format.** Existing text lists
are normalized into the same push/fetch path and promoted on a subsequent helper
push. Historical ciphertext stays at its original S3 keys: no full re-upload,
server-side copy, or re-encryption is required. Let old-helper uploads finish
before promotion. Historical larger bundles remain unchanged; only new bundles
use the smaller target. New ciphertext lives under the namespace's reserved
`.remote-aws-v2/bundles/PUSH_TIP/` prefix, and manifests reference exact keys.

Packing boundaries are not deterministic across different tips or local packing
states. Reuse adopts existing boundaries instead of recreating them. Retries can
reuse completed same-tip objects without transferring them again. Conditional
writes and push-tip metadata prevent replacing another push's ciphertext.

## Emergency recovery from S3

Self-contained manifests allow recovery without any DynamoDB access:

```sh
# List candidate manifest keys, branches, and tips.
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO

# Explicitly select one and recover into a nonexistent directory.
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO \
  --manifest NAMESPACE/.remote-aws-v2/repos/REPO/manifests/TIP-UUID.json \
  --directory /path/to/new-checkout
```

The normal private-key loader is still required. Recovery does not mutate AWS or
claim which push was last acknowledged. See [recovery details](storage.md#s3-only-emergency-recovery)
for legacy limitations and administrative pointer restoration.

## Upgrade existing tables

The lease-format upgrade is a hard cutover. Stop **all** old and new helpers
(including automated pushes) before migrating, and upgrade every client before
resuming. Old helpers cannot safely access migrated tables. The string `id`
partition key and S3 objects do not change; `branch` and `bundles` move into a
`data` map. Do not enable DynamoDB TTL on repository tables.

Build the upgraded helper without replacing the installed binary, then preview
one bucket's records:

```sh
go build -o /private/path/git-remote-aws .
/private/path/git-remote-aws --migrate-dynamolock \
  --account ACCOUNT_ID --region REGION --table TABLE --bucket BUCKET
```

Repeat with `--apply --writers-stopped --backup /private/path/metadata.json` to
migrate. The backup must be a new file; it stores the original DynamoDB JSON with
mode `0600` before any writes. The tool checks the account, schema, TTL, and every
selected record, conditionally updates unchanged records, then verifies the
result. It refuses uncleared locks, mixed schemas, and unknown fields. Already
migrated records are left alone; `--id BUCKET/REPOSITORY` limits the operation to
one repository using strongly consistent keyed reads, without scanning the table.
Retain backups until the upgraded clients have been verified.

## Rotation

Rerun the personal-file `--keygen` command above to extend both chains, then update
that recipient's line in each repository's `.publickeys` and commit it. **Do not
pass a repository's `.publickeys` to keygen.**

Retain all private generations for historical decryption. Adding/removing recipients
does not rewrite old ciphertext or revoke access to copies already obtained.
Keygen saves the private extension first: if publication fails, preserve both files
and reconcile their generations before retrying. Never discard an extra private key.

See [recipient key chains](https://github.com/nathants/go-libsodium#recipient-key-chains)
for format limits and loader details.

## Standalone encryption

```sh
export GIT_REMOTE_AWS_PUBLICKEY="$(cat ~/.config/git-remote-aws/public)"
echo hello | git-remote-aws --encrypt > ciphertext
git-remote-aws --decrypt < ciphertext
```

## Development

Cloud-free checks (CLI/terminal tests require Python 3.8+):

```sh
bash bin/check.sh
go test -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...
GOFLAGS=-race go test -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...
```

AWS tests require credentials and `GIT_REMOTE_AWS_TEST_{ACCOUNT,BUCKET,TABLE}`.
Use a disposable unversioned S3 bucket and DynamoDB table with string partition
key `id`, never production resources. Tests clean their objects/items; remove the
bucket/table afterward.

```sh
go test ./...
GOFLAGS=-race go test ./...
```

For historical compatibility tests, set `GIT_REMOTE_AWS_TEST_OLD_BINARY` to a
pre-keychain helper built with go-libsodium `v0.0.0-20260502104057-4e1a79aae4f3`.
Without that artifact, those tests are skipped.
