# Git-Remote-AWS

Encrypted Git hosting in S3, with DynamoDB compare-and-swap for concurrent pushes.
Each remote has one branch; force pushes are forbidden. Valid Git branch names
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
AWS permissions. Otherwise provision them beforehand. Clone and fetch work normally.

`.publickeys` contains one recipient per line, with rotations joined by colons:
`old:new:newest`. New bundles use each recipient's newest key from the pushed
commit. Commit recipient edits before pushing; staged or unstaged edits are rejected.
Rows need not be sorted; blank lines and a missing final newline are allowed.

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
