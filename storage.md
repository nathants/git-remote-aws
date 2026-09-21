# Shared namespace storage

## Identity and coordination

`aws://BUCKET+TABLE/NAMESPACE[/REPO]` identifies one immutable, single-branch
history. An omitted repo is exactly `1`, not the latest repo. Components start
with an ASCII letter or digit and otherwise contain letters, digits, dots,
underscores or hyphens. Names are case sensitive. There is no numeric-suffix
inference: `WickedEngine3` means `WickedEngine3/1`, not `WickedEngine/3`.

The default repo retains DynamoDB ID `BUCKET/NAMESPACE`; other repos use
`BUCKET/NAMESPACE/REPO`. Both default URL spellings acquire the same lease.
An existing literal legacy `NAMESPACE/1` record is an ambiguity and fails closed;
reconcile it administratively rather than silently merge two histories.

DynamoDB remains the publication authority and writer coordinator. Its existing
lease envelope and `data: {branch, bundles}` payload are unchanged. `bundles` is
an S3 manifest pointer. Each repo has its own lease, so separate repos can publish
concurrently. All protected work uses its lease context. S3 bundle creations are
conditional and immutable; no namespace-wide write lock is needed.

## Layout and promotion

Old objects remain at their original keys:

```text
NAMESPACE/0000..A
NAMESPACE/A..B
NAMESPACE/bundles_B
```

New writes use:

```text
NAMESPACE/.remote-aws-v2/bundles/PUSH_TIP/BASE..END
NAMESPACE/.remote-aws-v2/repos/REPO/manifests/TIP-UUID.json
```

The all-zero base has the repository's object-ID length. A version-2 manifest
contains its canonical repository name, branch, tip, Git object format, codec,
and a complete ordered bundle chain. Each descriptor records the exact S3 key,
range, ciphertext length, ETag, and sorted recipient fingerprints. It never
references another repo's manifest. New bundle keys include the original push's
final tip: identical intermediate ranges encrypted by different pushes cannot
overwrite each other. Conditional creation also protects concurrent same-tip
writers. Retries inspect completed same-tip objects before encrypting/uploading
again, even though Git may still need to recreate the temporary plaintext bundle.

Old text lists are normalized into the same in-memory manifest model. List and
fetch remain read-only. An upgraded push enriches legacy descriptors and publishes
JSON without moving, copying, or re-encrypting old bundles. A direct helper no-op
push can promote metadata too; ordinary `git push` may decide it is already up to
date and send no push command, in which case promotion waits for a subsequent
update. Newly created repos can adopt legacy bundles even before their source
repo is promoted. The ciphertext codec is unchanged.

**Upgrade all writers before publishing the new manifest format.** Old clients
cannot read its JSON manifests. Let an old-helper upload finish before promotion;
its completed bundle list can then be adopted without another full upload. The
separate lease-envelope migration is still required for tables that predate that
format; namespace promotion does not bypass it.

## Reuse and validation

An empty destination lists manifest candidates within its namespace, including
legacy cumulative lists. For each candidate it locates an ancestor endpoint of
the requested tip and considers the complete prefix through that endpoint. Among
compatible prefixes it chooses one minimizing the remaining commit count. It
adopts the stored boundaries, not a fresh split of the shared history. An existing
destination retains its own exact chain and still requires a fast-forward.

Validation checks:

- Version, codec, object format, literal branch, repository scope, and safe keys.
- One continuous chain from the all-zero base to the declared tip, without gaps,
  cycles, mixed object formats, or a range that fails local Git ancestry checks.
- Existence, size and ETag of every adopted object.
- Actual ciphertext recipient fingerprints, read with bounded, ETag-conditional
  range requests from the existing libsodium box envelope. Legacy policies are
  not guessed from `.publickeys` at an intermediate commit.

Within an adoption/promotion operation, each distinct stored object is inspected
once; contradictory descriptors remain fatal. Successful envelope validation is
cached in the Git-resolved `git-remote-aws/validation-v2/` metadata directory
(normally `.git/git-remote-aws/validation-v2/`, also supporting linked worktrees).
Each new operation still HEADs each distinct object and checks ETag/size before
using cached fingerprints. Entries are scoped to the resolved endpoint, bucket,
key, range, codec and object identity. Cache hits never replace ancestry/chain
checks or fetch authentication. Missing, corrupt or inaccessible local entries
are misses; missing or changed remote data remains an error. Cache writes are
atomic and best-effort; the directory can be deleted safely to force fresh header
reads. Like other local Git metadata, the cache is trusted local state, not a
security boundary against someone who can rewrite the checkout's metadata.

Cross-repo reuse requires one ciphertext recipient from each current public-key
chain and no extra recipients. Retained historical generations are compatible;
adding/removing recipients or replacing identities can force repacking rather
than silently adopt inaccessible or differently authorized ciphertext. Existing
repo history retains its historical key requirements, as before.

These checks are not a full payload audit: push does not download/decrypt all
historical Git data. Fetch pins reads to recorded ETags, checks length, authenticates
the stream, verifies the bundle head and Git prerequisites during import, and
checks the selected tip's object closure. A fresh clone followed by `git fsck
--strict` is a full end-to-end check. S3 write access is a trust boundary; unsigned
manifests and recipient routing fingerprints are not proof against a malicious
storage administrator.

A source manifest may disappear during discovery when another push supersedes
it. Only a typed missing-manifest response is skipped. Missing/changed bundle
objects, malformed manifests, and other discovery errors remain errors. If a
repo's DynamoDB pointer is lost but its S3 snapshots remain, a new push cannot
rewind or diverge from those snapshots; reconcile ambiguous history explicitly.
Complete snapshots left by unacknowledged initial pushes can be retried at the
same tip or extended, but conflicting snapshots require administrative attention.

## Sizing, retention, and permissions

New bundles default to a 256 MiB soft compressed target. Existing larger bundles
remain usable but indivisible for adoption. A rewrite inside an old large bundle
can require repacking its whole unshared suffix. Boundaries are not deterministic
across tips, Git versions, local packing, or size settings; reuse does not depend
on deterministic packing or encryption. Temporary storage remains bounded to one
bundle's plaintext/ciphertext pair.

Only superseded manifests are deleted, after confirmed publication of their
complete replacement. With bucket versioning enabled this adds delete markers,
preserving the old manifest versions for administrative recovery. An ambiguous
commit preserves both candidates. Bundles are never automatically deleted,
relocated, or garbage-collected. Manual deletion of a shared bundle can break
every manifest referencing it. Deleting one repo's DynamoDB record and
manifests does not grant ownership of its bundles.

Namespace discovery requires `s3:ListBucket` scoped to the namespace, and readers
need `s3:GetObject` for referenced keys, including original legacy locations.
Default-repo alias collision checks also read `BUCKET/NAMESPACE/1` in DynamoDB.
Existing conditional upload, multipart-abort, manifest-delete, and lease
permissions remain necessary. Every helper push checks/enables bucket versioning,
requiring `s3:GetBucketVersioning` and (only for enablement) `s3:PutBucketVersioning`.
See [setup and permissions](readme.md#usage) before upgrading writer permissions
or first enabling versioning. Resource setup does not alter IAM policies.

## S3-only emergency recovery

This recovers a checkout when DynamoDB metadata is lost but S3 objects survive.
It does not inspect historical S3 versions. If S3 data was deleted or overwritten,
restore its visibility administratively using versioning first.

List self-contained candidates without accessing DynamoDB:

```sh
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO
```

Each line contains the S3 key, branch, and tip. Select a manifest and recover into
a new directory that does not already exist:

```sh
git-remote-aws --recover --remote aws://BUCKET+TABLE/NAMESPACE/REPO \
  --manifest NAMESPACE/.remote-aws-v2/repos/REPO/manifests/TIP-UUID.json \
  --directory /path/to/new-checkout
```

Recovery uses only S3 reads and the normal private-key loader. It validates and
imports through the common fetch path, then checks out the selected tip. It never
writes S3 or DynamoDB, never chooses a latest candidate by timestamp, and does not
claim that the selected push was acknowledged. A failed recovery may leave its
new directory for inspection. Rebuilding the coordinating DynamoDB pointer is a
separate administrative action; stop writers before doing so. Pre-promotion text
lists lack branch metadata and are not self-contained recovery manifests.
