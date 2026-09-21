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
clients stopped. Follow the [upgrade guide](NINA.md#upgrading-existing-installations)
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
[setup and permissions](NINA.md#setup-and-permissions).

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
safety net, not guaranteed retention; see [versioning and retention](NINA.md#versioning-and-retention).

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
on both ancestry and recipient keys; see [shared storage](NINA.md#reuse-and-validation).

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
bundles are reused without resizing. See [sizing and upload details](NINA.md#bundle-sizing-and-uploads).

## Recovery

If DynamoDB metadata is lost, list available self-contained S3 manifests:

```sh
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE
```

Select a manifest explicitly and recover into a **nonexistent directory** using
`--manifest KEY --directory PATH`. Recovery requires your private keys, does not
write AWS metadata, and does not determine which push was last acknowledged.
It reads visible objects only, not historical S3 versions. Follow the
[recovery procedure](NINA.md#s3-only-emergency-recovery).

## Standalone encryption

```sh
export GIT_REMOTE_AWS_PUBLICKEY="$(cat ~/.config/git-remote-aws/public)"
echo hello | git-remote-aws --encrypt > ciphertext
git-remote-aws --decrypt < ciphertext
```

## Development

Run `GOTOOLCHAIN=local bash bin/check.sh` for analysis and cloud-free race tests.
See [NINA.md](NINA.md#validation) for prerequisites and live AWS tests.
