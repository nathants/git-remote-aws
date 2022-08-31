# git-remote-aws

## why

encrypted git hosting should be easy.

## how

encrypted git [bundles](https://git-scm.com/docs/git-bundle) are stored in s3.

compare and swap against dynamodb updates an ordered list of bundles. this enables multiple writers to safely collaborate on a single remote.

each remote can hold one and only one branch.

bundles in s3 are immutable, and force push is not allowed.

bundles are encrypted with libsodium [secretstream](https://doc.libsodium.org/secret-key_cryptography/secretstream). user keys are libsodium box [keypairs](https://doc.libsodium.org/public-key_cryptography/authenticated_encryption#key-pair-generation). authorized user public keys are added to a `.publickeys` file in the git repository. to add or remove authorized users, update the publickeys file, then create and push to a new remote or delete s3 data and recreate an existing remote.

metadata is stored unencrypted:
- branch name
- remote name
- git hash for the start and end of each bundle

data is stored encrypted:
- git bundles

both git sha1 and sha256 hashing algorithms are supported.

private s3 buckets and dynamodb tables are created ondemand if they do not already exist.

## what

a custom git remote adding support for remotes like:

`git remote add origin aws://${s3_bucket}+${dynamo_table}/${remote_name}`

the git remote binary provides a keygen for libsodium box [keypairs](https://doc.libsodium.org/public-key_cryptography/authenticated_encryption#key-pair-generation):

`git-remote-aws --keygen ~/.git-remote-aws/publickey ~/.git-remote-aws/secretkey`

the default path for your secret key is `~/.git-remote-aws/secretkey`. this can be changed via environment variable `GIT_REMOTE_AWS_SECRETKEY`.

## install

install go and libsodium from your package manager:

```bash
brew install         go     libsodium     # homebrew
sudo pacman -S       go     libsodium     # arch
sudo apk add         go     libsodium-dev # alpine
sudo apt-get install golang libsodium-dev # ubuntu/debian
```

install the binary and update PATH:

```bash
go install github.com/nathants/git-remote-aws@latest

export PATH=$PATH:$(go env GOPATH)/bin
```

## usage

```bash
>> git init

>> git remote add origin aws://${bucket}+${table}/myrepo

>> mkdir -p ~/.git-remote-aws

>> git-remote-aws --keygen ~/.git-remote-aws/publickey ~/.git-remote-aws/secretkey

>> cat ~/.git-remote-aws/publickey >> .publickeys

>> git add .

>> git commit -m init

>> git push -u origin master

creating private s3 bucket: $bucket
lib/s3.go:329: created bucket: $bucket
lib/s3.go:367: created bucket tags for: $bucket
lib/s3.go:415: created public access block for $bucket: private
lib/s3.go:657: created encryption for $bucket: true
lib/s3.go:688: put bucket metrics for: $bucket
created private s3 bucket: $bucket
creating private dynamodb table: $table
lib/dynamodb.go:481: created table: $table
lib/dynamodb.go:974: waiting for table active: $table
lib/dynamodb.go:974: waiting for table active: $table
created private dynamodb table: $table
get dynamodb://$table/$bucket/myrepo
get dynamodb://$table/$bucket/myrepo
get s3://$bucket/
git bundle: 0000000000000000000000000000000000000000..daf8ea23a2aa082a3eeffacbdda04917d14916cc
put s3://$bucket/myrepo/0000000000000000000000000000000000000000..daf8ea23a2aa082a3eeffacbdda04917d14916cc
put s3://$bucket/myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc
put dynamodb://$table/$bucket/myrepo
To aws://$bucket+$table/myrepo
 * [new branch]      master -> master

>> libaws s3-ls $bucket/ -r

770 myrepo/0000000000000000000000000000000000000000..daf8ea23a2aa082a3eeffacbdda04917d14916cc
 82 myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc

>> libaws dynamodb-item-scan $table | jq .

{
  "branch": "master",
  "bundles": "myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc",
  "id": "$bucket/myrepo",
  "uid": null,
  "unix": 0
}

>> cd $(mktemp -d)

>> git clone aws://${bucket}+${table}/myrepo

Cloning into 'myrepo'...
get dynamodb://$table/$bucket/myrepo
get s3://$bucket/myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc
get dynamodb://$table/$bucket/myrepo
get s3://$bucket/myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc
get s3://$bucket/myrepo/0000000000000000000000000000000000000000..daf8ea23a2aa082a3eeffacbdda04917d14916cc
git unbundle: 0000000000000000000000000000000000000000..daf8ea23a2aa082a3eeffacbdda04917d14916cc
get dynamodb://$table/$bucket/myrepo
get s3://$bucket/myrepo/bundles_daf8ea23a2aa082a3eeffacbdda04917d14916cc

```
