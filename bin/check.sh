#!/bin/bash
set -eou pipefail

which staticcheck >/dev/null   || (cd ~ && go install honnef.co/go/tools/cmd/staticcheck@latest)
which golint      >/dev/null   || (cd ~ && go install golang.org/x/lint/golint@latest)
which ineffassign >/dev/null   || (cd ~ && go install github.com/gordonklaus/ineffassign@latest)
which errcheck    >/dev/null   || (cd ~ && go install github.com/kisielk/errcheck@latest)
which bodyclose   >/dev/null   || (cd ~ && go install github.com/timakin/bodyclose@latest)
which nargs       >/dev/null   || (cd ~ && go install github.com/alexkohler/nargs/cmd/nargs@latest)
which go-hasdefault >/dev/null || (cd ~ && go install github.com/nathants/go-hasdefault@latest)
which go-hasdefer >/dev/null   || (cd ~ && go install github.com/nathants/go-hasdefer@latest)
which govulncheck >/dev/null || (cd ~ && go install golang.org/x/vuln/cmd/govulncheck@latest)

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
go vet -vettool=$(which bodyclose) main.go

echo go lint
golint ./... | grep -v -e unexported -e "should be" || true

echo static check
staticcheck main.go

echo ineffassign
ineffassign main.go

echo errcheck
errcheck main.go

echo go vet
go vet main.go

echo go build
go build -o /dev/null main.go
