# Git-Remote-AWS

Encrypted Git hosting in S3, coordinated by DynamoDB. Each remote has one branch;
force pushes are forbidden. Related repositories can share encrypted history
without sharing a branch tip. Supports Linux and SHA-1/SHA-256 Git repositories.

Git bundles use Libsodium encryption. Branch names, remote names, and bundle
boundary commit IDs are **not encrypted**. Encryption keys and AWS credentials
are separate.

## Install

Requires Go 1.27+, Libsodium development headers, and Git with
`rev-list --disk-usage` support.

```sh
go install github.com/nathants/git-remote-aws@latest
export PATH="$PATH:$(go env GOPATH)/bin"
```

**Upgrading an existing installation?** Upgrade all writers before publishing the
new manifest format. Older tables also require a coordinated migration with all
clients stopped. Follow the [upgrade guide](#upgrading-existing-installations)
before resuming use; historical encrypted bundles do not need re-uploading.

## Create your keys

```sh
mkdir -p -m 700 ~/.config/git-remote-aws
git-remote-aws --keygen \
  --public-key-file ~/.config/git-remote-aws/public \
  --secret-key-file ~/.config/git-remote-aws/secret
export GIT_REMOTE_AWS_SECRETKEY_FILE=~/.config/git-remote-aws/secret
```

Back up your private keys. They are required for cloning, fetching, and recovery.
Use exactly one nonempty secret source:

- `GIT_REMOTE_AWS_SECRETKEY_FILE`: private key file, mode `0600` or `0400`.
- `GIT_REMOTE_AWS_SECRETKEY`: private key chain text.
- `GIT_REMOTE_AWS_SECRETKEY_CMD`: executable that prints private chains; receives
  the remote URL as its argument. Interactive loaders are supported.

## Push and clone

Configure AWS credentials using standard AWS profiles or environment variables.
Use a provisioned S3 bucket and DynamoDB table, or use `ensure=y git push ...` to
create missing resources with suitable permissions. See
[setup and permissions](#setup-and-permissions).

```sh
mkdir myrepo && cd myrepo
git init -b main
cat ~/.config/git-remote-aws/public > .publickeys
git add .publickeys
git commit -m 'Add recipients'
git remote add origin aws://BUCKET+TABLE/NAMESPACE
git push -u origin main

# On another machine, after configuring AWS credentials and your private keys:
git clone aws://BUCKET+TABLE/NAMESPACE
```

**Push enables versioning for the entire bucket**, not just this repository.
Writers need `s3:GetBucketVersioning` and, when enablement is needed,
`s3:PutBucketVersioning`. Reads do not change versioning. Versioning is a recovery
safety net, not guaranteed retention. Superseded manifests are deleted with delete
markers; old versions remain available for administrative recovery. Keep
permanent-version-deletion permission out of everyday writer credentials. The
helper never restores versions, expires objects, or garbage-collects bundles;
shared bundles can be required by several repositories.

## Setup and permissions

`ensure=y` creates only missing resources; it never repairs existing configuration.
New buckets block public access, require HTTPS, use SSE-S3 with SSE-C blocked,
and enable versioning without expiration. New DynamoDB tables use on-demand
billing, a string partition key `id`, and disabled TTL and streams. **Never enable
TTL on repository tables.** Setup creates no credentials or IAM policies.

Serialize setup. If bucket creation fails after a possibly successful request,
inspect and complete setup administratively; do not blindly retry creation.

Normal use requires DynamoDB metadata reads and lease writes, plus namespace-scoped
S3 list/read/write access, `s3:AbortMultipartUpload`, and `s3:DeleteObject` for
superseded manifests. Reads must cover every referenced bundle, including shared
history. Pushes also require the bucket-versioning permissions described above.
Provisioning, migration, and test cleanup need additional administrative access.

## Related histories

URLs are `aws://BUCKET+TABLE/NAMESPACE[/REPO]`. An omitted repo means `/1`:
`project` and `project/1` are the same destination. Names are case sensitive;
`project3` is its own namespace, not shorthand for `project/3`.

To publish a rewritten history, choose a **new repo** in the same namespace:

```sh
git remote add rewritten aws://BUCKET+TABLE/project/2
git push -u rewritten main
```

Its first push reuses compatible ancestral bundles and uploads only the remaining
suffix. Existing destinations still require fast-forward updates. Reuse depends
on both ancestry and recipient keys: shared bundles must have one recipient per
current key chain and no extras. Retained key generations are compatible;
adding, removing, or replacing recipients can require repacking. Reuse validates
metadata and recipient envelopes; fetching authenticates the full ciphertext.

## Recipients and rotation

Commit `.publickeys` before pushing; staged or unstaged recipient edits are
rejected. It contains one recipient per line, with retained rotations joined by
colons: `old:new:newest`. New bundles use each recipient's newest committed key.

To rotate your key, rerun the personal-file `--keygen` command above, then update
your line in each repository's `.publickeys` and commit it. **Do not pass a
repository's `.publickeys` to keygen.**

Retain all private generations for historical decryption. Adding/removing
recipients does not rewrite old ciphertext or revoke access to copies already
obtained. If keygen reports a partial save, preserve both files and reconcile
their generations before retrying. See [key-chain details](https://github.com/nathants/go-libsodium#recipient-key-chains).

## Large pushes

New bundles target **256 MiB compressed**, configurable with:

```sh
git config remote-aws.bundleSize 256m
```

This is a soft target: an indivisible increment can exceed it. Allow temporary
disk space for both plaintext and ciphertext of the largest bundle. Existing
bundles are reused without resizing. If a push fails, rerun it: a retry of the
same commit validates and reuses the completed bundle prefix without repacking or
retransferring it. Changing the size setting affects only new bundles. Uploads
larger than 64 MiB use sequential multipart transfers. Interrupted uploads can
leave incomplete multipart uploads for administrative cleanup; do not delete
completed bundles merely because a push failed.

## Recovery

If DynamoDB metadata is lost, list available self-contained S3 manifests:

```sh
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE
```

Select a manifest explicitly and recover into a **nonexistent directory** using
`--manifest KEY --directory PATH`. Recovery requires your private keys, does not
write AWS metadata, and does not determine which push was last acknowledged.
It reads visible objects only, not historical S3 versions. Restore deleted or
overwritten objects administratively first. Legacy text manifests cannot be used
for this recovery; select a self-contained JSON manifest:

```sh
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE \
  --manifest NAMESPACE/.remote-aws-v2/repos/1/manifests/TIP-UUID.json \
  --directory /path/to/new-checkout
```

A failed recovery may leave its new directory for inspection. Rebuilding the
DynamoDB pointer is a separate administrative operation; stop writers first.

## Standalone encryption

```sh
export GIT_REMOTE_AWS_PUBLICKEY="$(cat ~/.config/git-remote-aws/public)"
echo hello | git-remote-aws --encrypt > ciphertext
git-remote-aws --decrypt < ciphertext
```

## Upgrading existing installations

Upgrade all writers before publishing JSON manifests. Promotion happens on a
subsequent helper push without rewriting encrypted bundles; Git can omit an
up-to-date push. Older tables independently require migration from flat records
to the go-dynamolock `data` envelope.

Stop **all** old and new clients before migrating, and upgrade every client before
resuming. Build without replacing the installed helper, then preview:

```sh
go build -o /private/path/git-remote-aws .
/private/path/git-remote-aws --migrate-dynamolock \
  --account ACCOUNT_ID --region REGION --table TABLE --bucket BUCKET
```

Repeat with `--apply --writers-stopped --backup /private/path/metadata.json` to
apply. The backup must be a new file, saved with mode `0600` before any writes;
retain it until upgraded clients are verified. `--id BUCKET/REPOSITORY` limits
migration to one record. Migration refuses uncleared locks, mixed schemas, and
unknown fields, leaves migrated records alone, and does not change S3 or the
string `id` key. Never enable TTL as part of migration.

## Development

Run `GOTOOLCHAIN=local bash bin/check.sh` for [libcheck](https://github.com/nathants/libcheck)
analysis and cloud-free race tests.
Compatibility tests build helpers from pinned historical commits with their own
module graphs, so run tests in a normal clone: shallow clones fail them, and the
first run needs module downloads or a warm module cache.
The gate requires Bash, Python 3.8+, and `libcheck` on PATH with its pinned tools;
its security scan needs network access. It never installs tools.

Live tests require credentials and a region for an independently known scratch
account. They fail rather than skip when the account guard is missing:

```sh
GIT_REMOTE_AWS_TEST_ACCOUNT="$EXPECTED_ACCOUNT" GOTOOLCHAIN=local GOFLAGS=-race \
  go test -count=1 -timeout=90m ./...
```

Each live test creates a unique bucket and table. Cleanup permanently deletes all
object versions, delete markers, and incomplete uploads in those test buckets.
The caller needs setup/read/write permissions and permission to list and delete
those objects, buckets, and tables. Tests also inspect bucket configuration, tags,
and table TTL. If a run is killed or times out, remove its leftover
`git-remote-aws-test-*` resources administratively.
