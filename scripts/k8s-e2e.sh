#!/usr/bin/env bash
# Phase 23b end-to-end run on a 2-node minikube cluster with Calico, so
# NetworkPolicies are enforced (docs/KUBERNETES.md, ADR-029 Rev 1.1).
# DEVELOPMENT ONLY: dev PostgreSQL, NATS, Fake ERP/MCP and dev secrets.
#
#   scripts/k8s-e2e.sh                 full run, then delete the profile
#   KEEP=1 scripts/k8s-e2e.sh          leave the cluster running
#   TESTS='TestSliceADemo' scripts/... choose the Go tests (default: Slice A, disruption, JIT, federated JIT, private_key_jwt, Vault, SPIFFE, token exchange, AWS, full Studio)
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
for img in eacp:dev eacp-agt-pdp:dev postgres:18-alpine nats:2.15.0-alpine busybox:1.37 hashicorp/vault:2.1.1 \
	ghcr.io/spiffe/spire-server:1.15.3 ghcr.io/spiffe/spire-agent:1.15.3 ghcr.io/spiffe/spiffe-csi-driver:0.2.13 \
	registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0; do
	docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
	minikube -p "$PROFILE" image load --overwrite=true "$img"
done

echo "==> Namespaces, dev secrets and dependencies"
k apply -f deployments/k8s/dev/namespaces.yaml
# ADR-019 Rev 1.4: the development SPIRE comes first, so Fake ERP starts with
# its bundle. The worker's entry: namespace eacp, ServiceAccount eacp-worker,
# JWT-SVIDs living an hour (twice the longest call budget and more).
k apply -f deployments/k8s/dev/spire.yaml
k -n spire rollout status deploy/spire-server --timeout=300s
spire() { k -n spire exec deploy/spire-server -- /opt/spire/bin/spire-server "$@"; }
entry() { # spiffe_id, then the entry create arguments
	local found
	found=$(spire entry show -spiffeID "$1") # a failure stops the script (set -e)
	case $found in "Found 0 entries"*) ;; *) return 0 ;; esac
	spire entry create -spiffeID "$1" "${@:2}"
}
entry spiffe://eacp.test/k8s-nodes -node -selector k8s_psat:cluster:eacp-e2e
entry spiffe://eacp.test/ns/eacp/sa/eacp-worker -parentID spiffe://eacp.test/k8s-nodes \
	-selector k8s:ns:eacp -selector k8s:sa:eacp-worker -jwtSVIDTTL 3600
work=$(mktemp -d)
pki=$(winpath "$work/pki") # kubectl and go on Windows need a native path
tunnel=
gateway_tunnel=
cleanup() {
	[ -n "$tunnel" ] && kill "$tunnel" 2>/dev/null || true
	[ -n "$gateway_tunnel" ] && kill "$gateway_tunnel" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
spire bundle show -format spiffe >"$work/spiffe-bundle.json"
EACP_ENV=development go run ./cmd/eacpctl pdp-dev-certs --dir "$pki" \
	--name eacp-pdp --name eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local
secret() { k -n "$1" create secret generic "$2" "${@:3}" --dry-run=client -o yaml | k apply -f -; }
secret eacp eacp-pdp-tls --from-file="$pki/ca.pem" --from-file="$pki/client.pem" \
	--from-file="$pki/client-key.pem" --from-file="$pki/server.pem" --from-file="$pki/server-key.pem"
secret eacp eacp-db-app --from-literal=url='postgres://eacp_app:eacp_app_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-db-owner --from-literal=url='postgres://eacp_owner:eacp_owner_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-nats-relay --from-literal=url='nats://relay:relay_dev@nats.eacp-deps.svc:4222'
secret eacp eacp-nats-worker --from-literal=url='nats://worker:worker_dev@nats.eacp-deps.svc:4222'
# A development private_key_jwt key (ADR-019 Rev 1.2): the worker gets it
# inline in its connector secrets, Fake ERP the public JWKS.
EACP_ENV=development go run ./cmd/eacpctl dev-client-key --dir "$(winpath "$work/client-key")" \
	--jwks "$(winpath "$work/client-jwks.json")"
# The worker's secrets on Kubernetes: the dev manifest plus the federated
# binding, whose client assertion is the worker's projected token (ADR-019
# Rev 1.1). Compose never sees that entry: it has no such token. The compose
# key files under /run/secrets/eacp-client-key/ become inline PEM, the
# vault block logs in with Kubernetes auth instead of AppRole (Rev 1.3), and
# the spiffe block and its bindings reach the development SPIRE (Rev 1.4) and
# the token-exchange bindings trade those identities at Fake ERP's STS (Rev 1.5),
# and the AWS bindings assume a role with them at its AWS STS (Rev 1.6).
"$python" - deployments/docker/secrets/connector-secrets.dev.json deployments/k8s/connector-secrets.federated.json \
	"$(winpath "$work/connector-secrets.json")" "$(winpath "$work/client-key")" deployments/k8s/connector-secrets.vault.json \
	deployments/k8s/connector-secrets.spiffe.json deployments/k8s/connector-secrets.exchange.json \
	deployments/k8s/connector-secrets.aws.json <<'PY'
import json, os, sys
merged = {"secrets": []}
for path in sys.argv[1:3]:
    merged["secrets"] += json.load(open(path, encoding="utf-8"))["secrets"]
merged["vault"] = json.load(open(sys.argv[5], encoding="utf-8"))["vault"]
spiffe = json.load(open(sys.argv[6], encoding="utf-8"))
merged["spiffe"] = spiffe["spiffe"]
merged["secrets"] += spiffe["secrets"]
for path in sys.argv[7:9]:
    merged["secrets"] += json.load(open(path, encoding="utf-8"))["secrets"]
prefix = "/run/secrets/eacp-client-key/"
for entry in merged["secrets"]:
    pk = entry.get("oauth2", {}).get("private_key_jwt")
    if not pk:
        continue
    for field, inline in (("key_file", "key"), ("certificate_file", "certificate")):
        if pk.get(field, "").startswith(prefix):
            name = pk.pop(field)[len(prefix):]
            pk[inline] = open(os.path.join(sys.argv[4], name), encoding="utf-8").read()
json.dump(merged, open(sys.argv[3], "w", encoding="utf-8"), indent=2)
PY
secret eacp eacp-connector-secrets --from-file=connector-secrets.json="$(winpath "$work/connector-secrets.json")"
secret eacp-deps postgres-bootstrap --from-literal=POSTGRES_PASSWORD=postgres \
	--from-literal=EACP_OWNER_PASSWORD=eacp_owner_dev --from-literal=EACP_APP_PASSWORD=eacp_app_dev
secret eacp-deps fakeerp-token --from-file=token=deployments/docker/secrets/fakeerp-token.dev
secret eacp-deps fakeerp-oauth-client --from-file=secret=deployments/docker/secrets/fakeerp-oauth-client.dev
secret eacp-deps fakemcp-token --from-file=token=deployments/docker/secrets/fakemcp-token.dev
secret eacp-deps fakemcp-hr-token --from-file=token=deployments/docker/secrets/fakemcp-hr-token.dev
secret eacp-deps fakellm-key --from-file=key=deployments/docker/secrets/fakellm-key.dev
secret eacp eacp-provider-keys --from-file=provider-secrets.json=deployments/docker/secrets/llm-secrets.generated.json
k -n eacp-deps create configmap fakemcp-hr-tools --from-file=tools.json=deployments/docker/fakemcp/hr-tools.json \
	--dry-run=client -o yaml | k apply -f -
k -n eacp-deps create configmap postgres-initdb --from-file=deployments/docker/postgres/initdb/01-roles.sh \
	--dry-run=client -o yaml | k apply -f -
# Fake ERP trusts the cluster's service-account issuer: its issuer and JWKS.
k get --raw /.well-known/openid-configuration >"$work/oidc.json"
k get --raw /openid/v1/jwks >"$work/jwks.json"
issuer=$("$python" -c 'import json,sys; print(json.load(open(sys.argv[1]))["issuer"])' "$(winpath "$work/oidc.json")")
k -n eacp-deps create configmap fakeerp-federation --from-literal=issuer="$issuer" \
	--from-file=jwks.json="$(winpath "$work/jwks.json")" \
	--from-file=client-jwks.json="$(winpath "$work/client-jwks.json")" \
	--from-file=spiffe-bundle.json="$(winpath "$work/spiffe-bundle.json")" --dry-run=client -o yaml | k apply -f -
k -n eacp-deps create configmap nats-config --from-file=nats.conf=deployments/docker/nats/nats.conf \
	--dry-run=client -o yaml | k apply -f -
k apply -f deployments/k8s/dev/
k -n eacp-deps rollout status statefulset/postgres --timeout=300s
for d in nats fakeerp fakemcp fakemcp-hr fakellm vault; do k -n eacp-deps rollout status "deploy/$d" --timeout=300s; done
k -n eacp-deps wait --for=condition=complete job/vault-init --timeout=300s
k -n agents rollout status deploy/agent --timeout=300s
for d in spire-agent spiffe-csi-driver; do k -n spire rollout status "daemonset/$d" --timeout=300s; done

echo "==> helm upgrade --install eacp"
"$HELM" upgrade --install eacp deployments/helm/eacp -n eacp -f deployments/k8s/e2e-values.yaml \
	-f deployments/k8s/studio-e2e-values.yaml \
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
	k -n eacp port-forward svc/eacp-llm-gateway --address 127.0.0.1 0:8083 >"$work/gateway-url" 2>"$work/gateway-tunnel.log" &
	gateway_tunnel=$!
	for _ in $(seq 90); do grep -q 'Forwarding from 127.0.0.1:' "$work/gateway-url" 2>/dev/null && break; sleep 1; done
	gateway_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$work/gateway-url" | head -n1)
	[ -n "$gateway_port" ] || { cat "$work/gateway-tunnel.log" >&2; exit 1; }
	status=0
	EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s EACP_DEMO_API="$api" EACP_DEMO_KUBE_CONTEXT="$PROFILE" EACP_DEMO_LLM="http://127.0.0.1:$gateway_port" \
		go test -count=1 -v -timeout 40m -run "${TESTS:-TestSliceADemo|TestKubernetesDisruption|TestJITDemo|TestFederatedJITDemo|TestPrivateKeyJWTDemo|TestVaultDemo|TestSPIFFEDemo|TestTokenExchangeDemo|TestAWSDemo|TestStudioDemo}" ./test/demo || status=$?
fi

if [ "${KEEP:-}" = 1 ]; then
	echo "==> Cluster $PROFILE left running"
else
	minikube delete -p "$PROFILE"
fi
exit "${status:-0}"
