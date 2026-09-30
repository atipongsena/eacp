#!/usr/bin/env bash
# Release binaries: every EACP command a deployment runs, for four
# platforms, with the version stamped in and a SHA256SUMS file.
#   scripts/ci/release-binaries.sh v0.1.0 [outdir]
set -euo pipefail
cd "$(dirname "$0")/../.."

version=${1:?usage: scripts/ci/release-binaries.sh <version> [outdir]}
out=${2:-dist}
commands="controlplane-api execution-worker llm-gateway agent-runtime eacpctl"
targets="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"
ldflags="-s -w -X github.com/atipongsena/eacp/internal/version.Version=$version"

python=python3
command -v python3 >/dev/null 2>&1 || python=python

rm -rf "$out"
mkdir -p "$out"
for target in $targets; do
	goos=${target%/*}
	goarch=${target#*/}
	name="eacp_${version}_${goos}_${goarch}"
	stage="$out/$name"
	mkdir -p "$stage"
	ext=""
	[ "$goos" = windows ] && ext=.exe
	for c in $commands; do
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags "$ldflags" -o "$stage/$c$ext" "./cmd/$c"
	done
	cp LICENSE NOTICE THIRD_PARTY_NOTICES.md "$stage/"
	if [ "$goos" = windows ]; then
		(cd "$out" && "$python" -m zipfile -c "$name.zip" "$name")
	else
		tar -C "$out" -czf "$out/$name.tar.gz" "$name"
	fi
	rm -rf "$stage"
done
(cd "$out" && sha256sum eacp_* > SHA256SUMS)
echo "release $version: $(ls "$out" | tr '\n' ' ')"
