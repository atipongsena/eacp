#!/usr/bin/env bash
# CI lint: formatting, vet, and a go.mod/go.sum that `go mod tidy` leaves alone.
set -euo pipefail
cd "$(dirname "$0")/../.."

unformatted=$(gofmt -l $(git ls-files '*.go'))
if [ -n "$unformatted" ]; then
	echo "gofmt would change:" >&2
	echo "$unformatted" >&2
	exit 1
fi
go vet ./...
go mod tidy
git diff --exit-code go.mod go.sum
