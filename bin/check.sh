#!/bin/bash
set -eou pipefail
cd "$(dirname "$0")/.."

command -v libcheck >/dev/null || {
    echo 'libcheck not found; install it with: go install github.com/nathants/libcheck@latest' >&2
    exit 1
}

echo libcheck
libcheck check

echo libcheck security
libcheck security

echo go build
go build -o /dev/null .

echo cloud-free race tests
GIT_REMOTE_AWS_TEST_ACCOUNT= GOFLAGS="${GOFLAGS:-} -race" go test -count=1 -timeout=5m -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...
