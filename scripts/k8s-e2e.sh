#!/usr/bin/env bash
# Phase 23b end-to-end run on a 2-node minikube cluster with Calico, so
# NetworkPolicies are enforced (docs/KUBERNETES.md, ADR-029 Rev 1.1).
# DEVELOPMENT ONLY: dev PostgreSQL, NATS, Fake ERP/MCP and dev secrets.
#
#   scripts/k8s-e2e.sh                 full run, then delete the profile
#   KEEP=1 scripts/k8s-e2e.sh          leave the cluster running
#   TESTS='TestSliceADemo' scripts/... choose the Go tests (default: both)
#   TESTS=NONE KEEP=1 scripts/...      install only
set -euo pipefail
cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: do not rewrite container paths

PROFILE=${PROFILE:-eacp-e2e}
HELM=${HELM:-}
for h in .tools/helm .tools/helm.exe helm; do
	[ -n "$HELM" ] && break
	command -v "$h" >/dev/null 2>&1 && HELM=$h
done
[ -n "$HELM" ] || {
	echo "helm not found: see docs/KUBERNETES.md (pinned Helm in .tools/)" >&2
	exit 2
}
k() { kubectl --context "$PROFILE" "$@"; }
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi; }
python=python3
command -v python3 >/dev/null 2>&1 || python=python

if ! minikube status -p "$PROFILE" >/dev/null 2>&1; then
	echo "==> Starting minikube profile $PROFILE (2 nodes, Calico)"
	minikube start -p "$PROFILE" --nodes 2 --cni calico --driver docker --cpus 3 --memory 2800
fi

echo "==> Building images and loading them into $PROFILE"
"$python" deployments/docker/secrets/prepare_fakeerp_token.py
docker build -q -t eacp:dev -f deployments/docker/Dockerfile . >/dev/null
docker build -q -t eacp-agt-pdp:dev -f sidecars/agt-pdp/Dockerfile --target runtime . >/dev/null
for img in eacp:dev eacp-agt-pdp:dev postgres:18-alpine nats:2.15.0-alpine busybox:1.37; do
	docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
	minikube -p "$PROFILE" image load --overwrite=true "$img"
done

echo "==> Namespaces, dev secrets and dependencies"
k apply -f deployments/k8s/dev/namespaces.yaml
work=$(mktemp -d)
pki=$(winpath "$work/pki") # kubectl and go on Windows need a native path
tunnel=
cleanup() {
	[ -n "$tunnel" ] && kill "$tunnel" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
EACP_ENV=development go run ./cmd/eacpctl pdp-dev-certs --dir "$pki" \
	--name eacp-pdp --name eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local
secret() { k -n "$1" create secret generic "$2" "${@:3}" --dry-run=client -o yaml | k apply -f -; }
secret eacp eacp-pdp-tls --from-file="$pki/ca.pem" --from-file="$pki/client.pem" \
	--from-file="$pki/client-key.pem" --from-file="$pki/server.pem" --from-file="$pki/server-key.pem"
secret eacp eacp-db-app --from-literal=url='postgres://eacp_app:eacp_app_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-db-owner --from-literal=url='postgres://eacp_owner:eacp_owner_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-nats-relay --from-literal=url='nats://relay:relay_dev@nats.eacp-deps.svc:4222'
secret eacp eacp-nats-worker --from-literal=url='nats://worker:worker_dev@nats.eacp-deps.svc:4222'
secret eacp eacp-connector-secrets \
	--from-file=connector-secrets.json=deployments/docker/secrets/connector-secrets.dev.json
secret eacp-deps postgres-bootstrap --from-literal=POSTGRES_PASSWORD=postgres \
	--from-literal=EACP_OWNER_PASSWORD=eacp_owner_dev --from-literal=EACP_APP_PASSWORD=eacp_app_dev
secret eacp-deps fakeerp-token --from-file=token=deployments/docker/secrets/fakeerp-token.dev
secret eacp-deps fakemcp-token --from-file=token=deployments/docker/secrets/fakemcp-token.dev
k -n eacp-deps create configmap postgres-initdb --from-file=deployments/docker/postgres/initdb/01-roles.sh \
	--dry-run=client -o yaml | k apply -f -
k -n eacp-deps create configmap nats-config --from-file=nats.conf=deployments/docker/nats/nats.conf \
	--dry-run=client -o yaml | k apply -f -
k apply -f deployments/k8s/dev/
k -n eacp-deps rollout status statefulset/postgres --timeout=300s
for d in nats fakeerp fakemcp; do k -n eacp-deps rollout status "deploy/$d" --timeout=300s; done
k -n agents rollout status deploy/agent --timeout=300s

echo "==> helm upgrade --install eacp"
"$HELM" upgrade --install eacp deployments/helm/eacp -n eacp -f deployments/k8s/e2e-values.yaml \
	--kube-context "$PROFILE" --wait --timeout 10m
k -n eacp get pods -o wide

if [ "${TESTS:-}" = NONE ]; then
	echo "==> Installed; no tests requested"
else
	echo "==> Tunnel to the API (through its Service, so it survives pod restarts)"
	minikube -p "$PROFILE" service eacp-api -n eacp --url >"$work/api-url" 2>"$work/tunnel.log" &
	tunnel=$!
	for _ in $(seq 90); do grep -q '^http' "$work/api-url" 2>/dev/null && break; sleep 1; done
	api=$(grep -m1 '^http' "$work/api-url") || {
		cat "$work/tunnel.log" >&2
		exit 1
	}
	echo "    API at $api"
	status=0
	EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s EACP_DEMO_API="$api" EACP_DEMO_KUBE_CONTEXT="$PROFILE" \
		go test -count=1 -v -timeout 40m -run "${TESTS:-TestSliceADemo|TestKubernetesDisruption}" ./test/demo || status=$?
fi

if [ "${KEEP:-}" = 1 ]; then
	echo "==> Cluster $PROFILE left running"
else
	minikube delete -p "$PROFILE"
fi
exit "${status:-0}"
