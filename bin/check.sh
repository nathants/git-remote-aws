#!/bin/bash
set -eou pipefail

for tool in staticcheck golint ineffassign errcheck bodyclose nargs go-hasdefault go-hasdefer govulncheck; do
    command -v "$tool" >/dev/null || {
        printf 'missing required check tool: %s\n' "$tool" >&2
        exit 1
    }
done

echo govulncheck
govulncheck ./...

echo go-hasdefault
go-hasdefault $(find -type f -name "*.go") || true

echo go-hasdefer
go-hasdefer $(find -type f -name "*.go") || true

echo go fmt
go fmt ./... >/dev/null

echo nargs
nargs ./...

echo bodyclose
go vet -vettool=$(which bodyclose) ./...

echo go lint
golint ./... | grep -v -e unexported -e "should be" || true

echo static check
staticcheck ./...

echo ineffassign
ineffassign ./...

echo errcheck
errcheck ./...

echo go vet
go vet ./...

echo go build
go build -o /dev/null .

echo cloud-free race tests
GIT_REMOTE_AWS_TEST_ACCOUNT= GOFLAGS="${GOFLAGS:-} -race" go test -count=1 -timeout=5m -run '^(TestKey|TestBundle|TestRef|TestEncryption|TestLease|TestMigration)' ./...
