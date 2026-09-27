# Running EACP on Kubernetes

The Helm chart `deployments/helm/eacp` runs the control-plane API, the execution worker and the AGT PDP sidecar
as replicated Deployments, with the same network boundary as compose (ADR-001 §3/§3a) and the availability
rules of ADR-029 (Rev 1.0 for the binaries, Rev 1.1 for the chart). PostgreSQL and NATS are **not** part of the
chart: use managed or HA services. The chart never holds a secret value.

## Prerequisites

- Kubernetes 1.30 or later, with a CNI that enforces NetworkPolicies (Calico, Cilium, …). Without one the
  policies below are silently ignored and the boundary is gone.
- Helm, pinned for this repository to **v4.3.0**. Put it in the git-ignored `.tools/` directory; the scripts and
  tests look there first, then on `PATH`:
  - Windows amd64: `https://get.helm.sh/helm-v4.3.0-windows-amd64.zip`, sha256
    `304ea163cce4d9ad14e189c01846c6a34de9cfdfe48536ae54b2e8ba7884e67c`; extract `helm.exe` to `.tools/`.
  - Other platforms: the matching archive from the same release, checked against its published `.sha256sum`.
- A dedicated namespace labelled `pod-security.kubernetes.io/enforce=restricted`. The chart's default-deny
  NetworkPolicy covers **every** pod in it.
- The images: `eacp` (`deployments/docker/Dockerfile`, all Go binaries) and `eacp-agt-pdp`
  (`sidecars/agt-pdp/Dockerfile --target runtime`), in a registry the cluster can pull from.

## Secrets

Create these Secrets yourself (or with your secret manager's operator). Values name them; the keys are fixed.

| Value | Keys | Reaches |
|---|---|---|
| `database.appSecret` | `url` — the `eacp_app` DSN | api, worker |
| `database.ownerSecret` | `url` — the `eacp_owner` DSN | the migrate Job only |
| `pdp.tlsSecret` | `ca.pem`, `client.pem`, `client-key.pem`, `server.pem`, `server-key.pem` | api: CA and client pair; pdp: CA and server pair |
| `nats.relaySecret` | `url` — the `relay` user | api (only with `nats.enabled`) |
| `nats.workerSecret` | `url` — the `worker` user | worker (only with `nats.enabled`) |
| `worker.connectorSecrets` | `connector-secrets.json` | worker only |

The PDP certificate must name the PDP Service (`<release>-pdp`, `<release>-pdp.<namespace>.svc`). For
development, `EACP_ENV=development eacpctl pdp-dev-certs --dir <dir> --name eacp-pdp --name
eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local` makes a throwaway PKI.

## Values that shape the boundary

- `database.peers`, `nats.peers` — NetworkPolicy peers selecting PostgreSQL and NATS (required).
- `worker.connectorEgress` — NetworkPolicy egress rules (`to` + `ports`) to the enterprise systems. It is the
  **only** way out to them. The chart refuses to render when it is empty, unless
  `worker.allowNoConnectorEgress=true`.
- Just-in-time credentials (ADR-019): an `oauth2` entry in `worker.connectorSecrets` makes the worker call its
  `token_url`, so that host belongs in `worker.connectorEgress` too, and so does the host of a token
  exchange's `impersonate.url` (Rev 1.5; a mint that cannot reach it logs `class=transport` with its
  `impersonation_host`), and so does an `aws` entry's STS host (Rev 1.6: `sts.<region>.amazonaws.com:443`
  unless `sts_endpoint` names another). The chart mounts only the manifest file,
  so give the client secret inline (`client_secret`); a `client_secret_file` path would not exist in the pod
  and the worker would refuse to start. A plain `http` token URL is accepted only when `environment` is
  `development` or `test`.
- `worker.workloadIdentity` (ADR-019 Rev 1.1) — `enabled` (default `false`), `audience` (required when
  enabled, 1–256 characters without whitespace) and `expirationSeconds` (600–86 400, default 3600). When
  enabled, the worker pod alone gets a projected service-account token, read-only at
  `/run/secrets/eacp-identity/token`; an `oauth2` entry names it as `client_assertion_file` and needs no
  client secret. The worker always runs as its own ServiceAccount, `<release>-worker`, so the federated
  subject `system:serviceaccount:<namespace>:<release>-worker` names only the worker. For Entra ID: create a
  federated identity credential on the application with the cluster's service-account issuer (its OIDC
  issuer URL, which Entra must be able to reach), that subject and the audience `api://AzureADTokenExchange`
  (set `audience` to the same value); the token URL is
  `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token`, and its host belongs in
  `worker.connectorEgress`. The Azure Workload Identity webhook is not needed: the chart projects the token.
- `private_key_jwt` (ADR-019 Rev 1.2) needs no chart value: put the worker's key and certificate inline in its
  `oauth2` entry (`"private_key_jwt": {"alg": "PS256", "key": "-----BEGIN PRIVATE KEY-----\n…",
  "certificate": "…"}`), since the chart mounts only the manifest file and a `key_file` path would not exist
  in the pod. The key lives only in the worker's connector-secrets Secret; register the certificate (Entra ID)
  or the public JWKS (Okta, Keycloak) with the IdP. `scripts/k8s-e2e.sh` generates a development key with
  `eacpctl dev-client-key`, inlines it this way and gives Fake ERP the JWKS through the `fakeerp-federation`
  ConfigMap (`client-jwks.json`).
- `worker.vaultIdentity` (ADR-019 Rev 1.3) — `enabled` (default `false`), `audience` (default `vault`, 1–256
  characters without whitespace) and `expirationSeconds` (600–86 400, default 3600). When enabled, the worker
  pod alone gets a second projected service-account token, read-only at
  `/run/secrets/eacp-vault-identity/token`, for Vault's Kubernetes auth: the secrets file's `vault` block names
  it as `"auth": {"kubernetes": {"role": "…", "jwt_file": "/run/secrets/eacp-vault-identity/token"}}`. It is
  separate from `workloadIdentity`'s token, so neither the ERP's IdP nor Vault can replay a token meant for the
  other. Bind the Vault role to service account `<release>-worker` in the release namespace and the same
  audience, and list Vault's address in `worker.connectorEgress`. A Vault running outside the cluster reviews
  the token with a reviewer JWT or the cluster's issuer; one inside it needs `system:auth-delegator` (as
  `deployments/k8s/dev/vault.yaml` does).
- `worker.spiffe` (ADR-019 Rev 1.4) — `enabled` (default `false`) and `csiDriver` (default `csi.spiffe.io`; an
  empty or whitespace name is refused at render time). When enabled, the worker pod alone gets an inline,
  read-only `csi` volume from the SPIFFE CSI driver at `/spiffe-workload-api`, where the SPIRE agent's socket
  appears as `spire-agent.sock`: the secrets file's `spiffe` block names it as
  `"endpoint": "unix:///spiffe-workload-api/spire-agent.sock"`. A `csi` volume is allowed by the `restricted`
  Pod Security Standard (a hostPath socket is not) and needs no egress rule. SPIRE, its agents and the CSI
  driver are yours to run; register the worker as `k8s:ns:<namespace>` and `k8s:sa:<release>-worker` with a
  JWT-SVID TTL of at least 2.5 × (the longest call budget + 30 s) (ADR-019 §3d).
- `api.ingress.from` — who may reach the API on 8080. Default `[]`: any source, port 8080 only (the API
  authenticates every call). Narrow it to your ingress controller and agent namespaces.
- `otel.peers` / `otel.ports` — optional egress for the OTLP exporter.
- `environment` — `EACP_ENV` of the API and the worker, default `production`. Outside `development` and `test`
  the NATS URL must be `tls://` (ADR-014 §6). The binaries alone default to `development`; the chart does not.
- `nats.caSecret`, `database.caSecret` — optional Secrets with key `ca.pem` for a private CA. The NATS CA is
  mounted into the API and the worker as `EACP_NATS_CA_FILE`; the PostgreSQL CA is mounted at
  `/run/secrets/eacp-db/ca.pem` in the API, the worker and the migrate Job — point the DSNs at it with
  `sslrootcert=/run/secrets/eacp-db/ca.pem`.

The chart fails at render time (`helm template` / `install` stops with a message) when:

- a required Secret name is missing, or PostgreSQL/NATS peers or the connector egress are empty (see above);
- any `*.env` sets `EACP_CONNECTOR_SECRETS_FILE` (only `worker.connectorSecrets` provides it) or
  `EACP_DATABASE_URL`, holds a URL with credentials (`scheme://user@…` or `scheme://user:password@…`), holds a
  non-string value, or sets a variable the chart itself sets (`EACP_ENV`, `EACP_HTTP_ADDR`, `EACP_LOG_FORMAT`,
  `EACP_WORKER_ID`, `EACP_GOVERNANCE_PROVIDER`, `EACP_NATS_URL`, `EACP_NATS_CA_FILE`, `EACP_SHUTDOWN_*`,
  `EACP_AGT_PDP_*`, `AGT_PDP_*`) — Kubernetes keeps the last of two definitions, so an override would win;
- `environment` is not `development`, `test`, `staging` or `production`;
- `shutdown.delay` is not whole seconds from `0s` to `60s`, `shutdown.timeout` is not positive whole seconds, or
  `worker.maxCallSeconds` is not a whole number of at least 1;
- replicas are below 1 or a grace period is too short (below);
- the governance provider is not `microsoft-agt` (or `local` with `governance.allowLocal`, for tests).

## Install and upgrade

```bash
kubectl create namespace eacp
kubectl label namespace eacp pod-security.kubernetes.io/enforce=restricted
# create the Secrets above, then:
.tools/helm upgrade --install eacp deployments/helm/eacp -n eacp -f my-values.yaml --wait
```

Migrations run as a Helm hook Job (`pre-install,pre-upgrade`) with the owner DSN, before any new pod starts.
A failed migration fails the install or upgrade and leaves the running pods untouched.

## What the NetworkPolicies allow

| Pod | Ingress | Egress |
|---|---|---|
| every chart pod | — | DNS (kube-system `k8s-app=kube-dns`, 53 UDP/TCP) |
| api | 8080 from `api.ingress.from` | PostgreSQL, NATS, PDP 8443, OTLP |
| worker | none (kubelet probes only) | PostgreSQL, NATS, `worker.connectorEgress`, OTLP |
| pdp | 8443 from api pods only | none |
| migrate Job | none | PostgreSQL |

Everything else in the namespace is denied both ways. An agent namespace reaches only the API; your enterprise
systems should also accept only the worker, as the dev Fake ERP does.

## Availability

- Replicas: 2 each by default; `EACP_WORKER_ID` is the pod name. No leader (ADR-029 §1).
- PodDisruptionBudgets: `maxUnavailable: 1` for api, worker and pdp.
- Rolling updates: `maxUnavailable: 0`, `maxSurge: 1`; pods spread across nodes (`ScheduleAnyway`).
- Shutdown: `EACP_SHUTDOWN_DELAY` (default 10 s) keeps a pod serving but not ready while endpoints update;
  `EACP_SHUTDOWN_TIMEOUT` (15 s) bounds the graceful HTTP shutdown. The PDP gets the same values as
  `AGT_PDP_SHUTDOWN_DELAY` and `AGT_PDP_SHUTDOWN_TIMEOUT`: it keeps accepting, closing each connection after its
  answer, until kube-proxy on every node has stopped sending it new connections, then finishes the decisions
  it accepted (ADR-029 Rev 1.2). The chart refuses a worker `terminationGracePeriodSeconds` below delay +
  timeout + `worker.maxCallSeconds` and an API or PDP (`pdp.terminationGracePeriodSeconds`, default 30) grace
  below delay + timeout. `worker.maxCallSeconds` defaults to 300, the longest call PostgreSQL allows (a
  contract's `timeout_ms` is capped at 300 s), so the default worker grace is 330 s. The grace is only an upper bound: a
  worker exits as soon as its in-flight calls finish. Lower it only if every contract's timeout is shorter: a
  worker killed mid-call leaves an unknown outcome that reconciliation must settle.
- DNS: the API and the worker run with the pod DNS options `timeout:1` and `attempts:3`, so a DNS query lost
  while pods churn is resent after 1 s; Go's default, 5 s, is the whole budget of a PDP call (ADR-029 Rev 1.2).
- Probes: `/readyz` and `/healthz` for api and worker. The PDP's HTTP health needs a client certificate, so its
  probes are TCP connects on 8443; each one logs a `connection_error` (`SSLEOFError`) line in the PDP. That is
  the probe, not a client.

## Autoscaling

`autoscaling.enabled=true` adds CPU HorizontalPodAutoscalers for the API and the worker (defaults 2–6 and 2–10
at 70 %). They are off by default: throughput is bounded by PostgreSQL admission and `max_inflight`, and worker
CPU does not follow the queue. Queue-depth scaling (KEDA or an external-metrics adapter) is deferred (ADR-029
Rev 1.1).

## The end-to-end run (development only)

`scripts/k8s-e2e.sh` proves the chart on a real 2-node minikube cluster with Calico, so the NetworkPolicies are
enforced:

1. starts profile `eacp-e2e` (2 nodes, 3 CPUs and 2 800 MB each, Docker driver) unless it exists;
2. builds `eacp:dev` and `eacp-agt-pdp:dev` and loads them and the dev images into the cluster (nothing is
   pulled inside it);
3. installs a DEVELOPMENT-ONLY SPIRE 1.15.3 in namespace `spire` (`deployments/k8s/dev/spire.yaml`: one server
   with SQLite on an emptyDir pinned to the control-plane node, trust domain `eacp.test`, an agent per node and
   the SPIFFE CSI driver), registers the worker as `spiffe://eacp.test/ns/eacp/sa/eacp-worker` (JWT-SVID TTL
   3600 s) and snapshots the trust bundle for Fake ERP;
4. creates namespaces `eacp` (restricted), `eacp-deps` and `agents`, the dev Secrets (from
   `deployments/docker/secrets` and `eacpctl pdp-dev-certs`) and the dev dependencies of
   `deployments/k8s/dev` — PostgreSQL, NATS, Fake ERP, Fake MCP, a dev-mode Vault with its init Job
   (Kubernetes auth for the worker) and a busybox stand-in agent, each protecting itself with its own
   NetworkPolicies, all pinned to the control-plane node;
5. `helm upgrade --install eacp … -f deployments/k8s/e2e-values.yaml --wait`;
6. opens `minikube service eacp-api --url` (through the Service, so it survives pod restarts) and runs
   `TestSliceADemo`, `TestKubernetesDisruption` and the credential demos (`TestJITDemo`,
   `TestFederatedJITDemo`, `TestPrivateKeyJWTDemo`, `TestVaultDemo`, `TestSPIFFEDemo`, `TestTokenExchangeDemo`, `TestAWSDemo`) from `test/demo` with
   `EACP_DEMO_PLATFORM=k8s`;
7. deletes the profile.

```bash
bash scripts/k8s-e2e.sh                      # full run, then delete the cluster
KEEP=1 bash scripts/k8s-e2e.sh               # leave it running
TESTS=TestSliceADemo bash scripts/k8s-e2e.sh # choose the tests
TESTS=NONE KEEP=1 bash scripts/k8s-e2e.sh    # install only
```

`TestKubernetesDisruption` submits 30 purchases (half of them slow calls, in flight while pods move), each on a
new connection. Meanwhile it rolls the API and the PDP with the control-plane node cordoned, so every replica
lands on the second node, scales the workers to 3 and drains that node, whose API and PDP evictions must then
pass their PodDisruptionBudgets one at a time. Every purchase must be accepted on the first attempt and end
`SUCCEEDED` with exactly one ERP record; neither the API nor the PDP Service may ever have no ready endpoint
(an EndpointSlice watch); a `/healthz` probe every 100 ms through the Service must never fail; the stand-in
agent must reach the API and nothing else (PDP, PostgreSQL, NATS, Fake ERP, a worker pod); and every EACP pod
must run non-root with a read-only root filesystem in a namespace that enforces `restricted`. Without the
PodDisruptionBudgets the drain evicts both PDP pods at once and the run fails. Without the PDP's shutdown delay
it fails too, in most attempts: kube-proxy still routes a new connection to a PDP pod that got SIGTERM and has
already closed its listener, and one purchase gets `503 governance_unavailable`. Without the DNS options it
fails now and then the same way, when a DNS query lost during the churn stalls the API's lookup of the PDP.

The Slice C demo copies a file into the distroless Fake MCP pod, so it runs on compose only
(`scripts/demo.sh`); on Kubernetes it skips.

The render tests run without a cluster: `EACP_HELM_REQUIRED=1 go test ./test/helm` (without the variable they
skip when Helm is missing).

## Out of scope

Queue-depth autoscaling, cert-manager, a service mesh, PostgreSQL or NATS operators or HA, Ingress or Gateway
objects, publishing images, multi-cluster, and Slice C on Kubernetes.
