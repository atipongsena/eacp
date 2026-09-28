#!/usr/bin/env bash
# CI Helm chart render tests with the pinned Helm (docs/KUBERNETES.md).
set -euo pipefail
cd "$(dirname "$0")/../.."

version=v4.3.0
if [ ! -x .tools/helm ] && [ ! -x .tools/helm.exe ]; then
	case "$(uname -s)-$(uname -m)" in
	Linux-x86_64) archive=helm-$version-linux-amd64.tar.gz ;;
	*) echo "download Helm $version for this platform into .tools/ (docs/KUBERNETES.md)" >&2; exit 1 ;;
	esac
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/$archive" "https://get.helm.sh/$archive"
	curl -fsSL -o "$tmp/$archive.sha256sum" "https://get.helm.sh/$archive.sha256sum"
	(cd "$tmp" && sha256sum -c "$archive.sha256sum")
	tar -xzf "$tmp/$archive" -C "$tmp"
	mkdir -p .tools
	install -m 0755 "$tmp/linux-amd64/helm" .tools/helm
	rm -rf "$tmp"
fi
EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm
